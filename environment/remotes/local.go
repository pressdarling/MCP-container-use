package remotes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"dagger.io/dagger"
	"github.com/mitchellh/go-homedir"

	"github.com/dagger/container-use/environment"
)

const (
	containerUseRemote = "container-use"
	gitNotesLogRef     = "container-use"
	gitNotesStateRef   = "container-use-state"

	// Config directory paths
	configBaseDir              = "~/.config/container-use"
	configRepoPathTemplate     = configBaseDir + "/repos/%s"
	configWorktreePathTemplate = configBaseDir + "/worktrees/%s"
)

const maxFileSizeForTextCheck = 10 * 1024 * 1024

// LocalRemote implements the Remote interface using local filesystem and git
type LocalRemote struct {
	dag *dagger.Client
}

// NewLocalRemote creates a new LocalRemote instance
func NewLocalRemote(dag *dagger.Client) *LocalRemote {
	return &LocalRemote{dag: dag}
}

// storage represents the local storage for an environment
type storage struct {
	repoPath     string
	worktreePath string
	branch       string
}

// RemoteUrl returns the file:// URL for the project storage, ensuring the remote exists
func (r *LocalRemote) RemoteUrl(project string) string {
	ctx := context.Background()

	// Get absolute path of the project
	projectPath, err := filepath.Abs(project)
	if err != nil {
		return ""
	}

	// Initialize the remote storage if it doesn't exist
	repoPath, err := initializeLocalRemote(ctx, projectPath)
	if err != nil {
		return ""
	}

	return "file://" + repoPath
}

// Create initializes storage for a new environment
func (r *LocalRemote) Create(env *environment.Environment) error {
	ctx := context.Background()

	localRepoPath, err := filepath.Abs(env.Source())
	if err != nil {
		return err
	}

	cuRepoPath, err := initializeLocalRemote(ctx, localRepoPath)
	if err != nil {
		return err
	}

	worktreePath, err := getWorktreePath(env.ID)
	if err != nil {
		return err
	}

	s := &storage{
		repoPath:     cuRepoPath,
		worktreePath: worktreePath,
		branch:       env.ID,
	}

	// Determine the default branch
	defaultBranch, err := runGitCommand(ctx, localRepoPath, "branch", "--show-current")
	if err != nil || strings.TrimSpace(defaultBranch) == "" {
		// Try to get the default branch name
		defaultBranch, err = runGitCommand(ctx, localRepoPath, "symbolic-ref", "refs/remotes/origin/HEAD")
		if err != nil {
			defaultBranch = "main" // fallback
		} else {
			defaultBranch = strings.TrimPrefix(strings.TrimSpace(defaultBranch), "refs/remotes/origin/")
		}
	} else {
		defaultBranch = strings.TrimSpace(defaultBranch)
	}

	if err := s.createWorktree(ctx, defaultBranch); err != nil {
		return err
	}

	return nil
}

// Save saves the environment state and commits changes
func (r *LocalRemote) Save(env *environment.Environment, commitName, commitDescription string) error {
	ctx := context.Background()
	s, err := r.getStorage(env)
	if err != nil {
		return err
	}

	if err := s.save(ctx, env); err != nil {
		return err
	}

	if err := s.commitStateToNotes(ctx, env); err != nil {
		return err
	}

	name := commitName
	if name == "" {
		name = "Auto-save"
	}

	description := commitDescription
	if description == "" {
		description = "Automatic save"
	}

	return s.commitChanges(ctx, name, description)
}

// Note adds a note to the environment
func (r *LocalRemote) Note(env *environment.Environment, note string) error {
	ctx := context.Background()
	s, err := r.getStorage(env)
	if err != nil {
		return err
	}

	return s.addGitNote(ctx, note)
}

// Patch applies uncommitted changes to the environment
func (r *LocalRemote) Patch(env *environment.Environment, patch string) error {
	ctx := context.Background()
	s, err := r.getStorage(env)
	if err != nil {
		return err
	}

	return s.applyPatch(ctx, patch)
}

// Load loads the environment state from storage
func (r *LocalRemote) Load(env *environment.Environment) error {
	s, err := r.getStorage(env)
	if err != nil {
		return err
	}

	return s.load(env)
}

// Delete removes the environment from storage
func (r *LocalRemote) Delete(repoName string, envName string) error {
	ctx := context.Background()
	var lastErr error

	// Get the repo path first
	repoPath, err := getRepoPath(repoName)
	if err != nil {
		return fmt.Errorf("failed to get repo path: %w", err)
	}

	// Delete the worktree
	worktreePath, err := getWorktreePath(envName)
	if err != nil {
		return fmt.Errorf("failed to expand worktree path: %w", err)
	}

	// Check if worktree exists before trying to delete it
	if _, err := os.Stat(worktreePath); err == nil {
		slog.Info("Deleting environment worktree", "envName", envName, "path", worktreePath)
		if err := os.RemoveAll(worktreePath); err != nil {
			slog.Warn("Failed to delete worktree", "path", worktreePath, "err", err)
			lastErr = err
		}
	} else if !os.IsNotExist(err) {
		slog.Warn("Failed to check worktree existence", "path", worktreePath, "err", err)
		lastErr = err
	} else {
		slog.Info("Worktree already deleted", "envName", envName, "path", worktreePath)
	}

	// Check if repo exists before running git commands
	if _, err := os.Stat(repoPath); err == nil {
		// Prune worktree references after manual removal
		slog.Info("Pruning worktree references", "repo", repoPath)
		_, err = runGitCommand(ctx, repoPath, "worktree", "prune")
		if err != nil {
			slog.Warn("Failed to prune worktree references", "repo", repoPath, "err", err)
			if lastErr == nil {
				lastErr = err
			}
		}

		// Delete the environment branch from the specified storage repo
		slog.Info("Deleting environment branch", "envName", envName, "repo", repoPath)
		_, err = runGitCommand(ctx, repoPath, "branch", "-D", envName)
		if err != nil {
			// Check if error is because branch doesn't exist
			if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "does not exist") {
				slog.Info("Environment branch already deleted", "envName", envName, "repo", repoPath)
			} else {
				slog.Warn("Failed to delete environment branch", "envName", envName, "repo", repoPath, "err", err)
				if lastErr == nil {
					lastErr = err
				}
			}
		}
	} else if !os.IsNotExist(err) {
		slog.Warn("Failed to check repo existence", "repo", repoPath, "err", err)
		if lastErr == nil {
			lastErr = err
		}
	} else {
		slog.Info("Repository already deleted", "repo", repoPath)
	}

	// Return the last error encountered, if any, but don't fail the operation
	// since this is a cleanup operation and partial success is acceptable
	if lastErr != nil {
		slog.Warn("Delete operation completed with some errors", "lastErr", lastErr)
	}

	return nil
}

// BaseProjectDir returns the base project directory for building containers
func (r *LocalRemote) BaseProjectDir(env *environment.Environment) *dagger.Directory {
	s, err := r.getStorage(env)
	if err != nil {
		return nil
	}

	return r.dag.Host().Directory(s.worktreePath)
}

// getStorage creates a storage instance for the environment
func (r *LocalRemote) getStorage(env *environment.Environment) (*storage, error) {
	repoName, err := getRepoName(env.Source())
	if err != nil {
		return nil, err
	}

	cuRepoPath, err := getRepoPath(repoName)
	if err != nil {
		return nil, err
	}

	worktreePath, err := getWorktreePath(env.ID)
	if err != nil {
		return nil, err
	}

	return &storage{
		repoPath:     cuRepoPath,
		worktreePath: worktreePath,
		branch:       env.ID,
	}, nil
}

func getRepoPath(repoName string) (string, error) {
	return homedir.Expand(fmt.Sprintf(configRepoPathTemplate, filepath.Base(repoName)))
}

func getWorktreePath(envName string) (string, error) {
	return homedir.Expand(fmt.Sprintf(configWorktreePathTemplate, envName))
}

func getRepoName(sourcePath string) (string, error) {
	absPath, err := filepath.Abs(sourcePath)
	if err != nil {
		return "", err
	}
	return filepath.Base(absPath), nil
}

func initializeLocalRemote(ctx context.Context, localRepoPath string) (string, error) {
	localRepoPath, err := filepath.Abs(localRepoPath)
	if err != nil {
		return "", err
	}

	// Check if localRepoPath is a git repository, initialize if not
	if _, err := os.Stat(filepath.Join(localRepoPath, ".git")); os.IsNotExist(err) {
		slog.Info("Initializing git repository", "path", localRepoPath)
		_, err = runGitCommand(ctx, localRepoPath, "init")
		if err != nil {
			return "", err
		}

		// Create an initial commit if the repo is empty
		_, err = runGitCommand(ctx, localRepoPath, "commit", "--allow-empty", "-m", "Initial commit")
		if err != nil {
			return "", err
		}
	}

	repoName, err := getRepoName(localRepoPath)
	if err != nil {
		return "", err
	}
	cuRepoPath, err := getRepoPath(repoName)
	if err != nil {
		return "", err
	}

	if _, err := os.Stat(cuRepoPath); err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}

		slog.Info("Initializing storage repository", "source", localRepoPath, "storage", cuRepoPath)
		_, err = runGitCommand(ctx, localRepoPath, "clone", "--bare", localRepoPath, cuRepoPath)
		if err != nil {
			return "", err
		}
	}

	// Set up remote in source repo pointing to storage
	existingURL, err := runGitCommand(ctx, localRepoPath, "remote", "get-url", containerUseRemote)
	if err != nil {
		_, err = runGitCommand(ctx, localRepoPath, "remote", "add", containerUseRemote, cuRepoPath)
		if err != nil {
			return "", err
		}
	} else {
		existingURL = strings.TrimSpace(existingURL)
		if existingURL != cuRepoPath {
			_, err = runGitCommand(ctx, localRepoPath, "remote", "set-url", containerUseRemote, cuRepoPath)
			if err != nil {
				return "", err
			}
		}
	}
	return cuRepoPath, nil
}

func runGitCommand(ctx context.Context, dir string, args ...string) (out string, rerr error) {
	slog.Info(fmt.Sprintf("[%s] $ git %s", dir, strings.Join(args, " ")))
	defer func() {
		slog.Info(fmt.Sprintf("[%s] $ git %s (DONE)", dir, strings.Join(args, " ")), "err", rerr)
	}()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir

	output, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("git command failed (exit code %d): %w\nOutput: %s",
				exitErr.ExitCode(), err, string(output))
		}
		return "", fmt.Errorf("git command failed: %w", err)
	}

	return string(output), nil
}

func (s *storage) createWorktree(ctx context.Context, sourceBranch string) error {
	if _, err := os.Stat(s.worktreePath); err == nil {
		return nil
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(s.worktreePath), 0755); err != nil {
		return err
	}

	// Create worktree from storage repo - check if our target branch already exists
	_, err := runGitCommand(ctx, s.repoPath, "show-ref", "--verify", "--quiet", fmt.Sprintf("refs/heads/%s", s.branch))
	if err != nil {
		// Branch doesn't exist, create it from sourceBranch
		_, err = runGitCommand(ctx, s.repoPath, "worktree", "add", "-b", s.branch, s.worktreePath, sourceBranch)
		if err != nil {
			return err
		}
	} else {
		// Branch exists, checkout to it
		_, err = runGitCommand(ctx, s.repoPath, "worktree", "add", s.worktreePath, s.branch)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *storage) save(ctx context.Context, env *environment.Environment) error {
	_, err := env.Container().Directory(env.Workdir).Export(
		ctx,
		s.worktreePath,
		dagger.DirectoryExportOpts{Wipe: true},
	)
	if err != nil {
		return err
	}

	cfg := filepath.Join(s.worktreePath, ".container-use")
	if err := os.MkdirAll(cfg, 0755); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(cfg, "AGENT.md"), []byte(env.Instructions), 0644); err != nil {
		return err
	}

	envState, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(cfg, "environment.json"), envState, 0644); err != nil {
		return err
	}

	return nil
}

func (s *storage) load(env *environment.Environment) error {
	cfg := filepath.Join(s.worktreePath, ".container-use")

	instructions, err := os.ReadFile(filepath.Join(cfg, "AGENT.md"))
	if err != nil {
		return err
	}
	env.Instructions = string(instructions)

	envState, err := os.ReadFile(filepath.Join(cfg, "environment.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(envState, env); err != nil {
		return err
	}

	return nil
}

func (s *storage) commitChanges(ctx context.Context, name, explanation string) error {
	status, err := runGitCommand(ctx, s.worktreePath, "status", "--porcelain")
	if err != nil {
		return err
	}

	if strings.TrimSpace(status) == "" {
		return nil
	}

	if err := addNonBinaryFiles(ctx, s.worktreePath); err != nil {
		return err
	}

	commitMsg := fmt.Sprintf("%s\n\n%s", name, explanation)
	_, err = runGitCommand(ctx, s.worktreePath, "commit", "-m", commitMsg)
	return err
}

func (s *storage) commitStateToNotes(ctx context.Context, env *environment.Environment) error {
	buff, err := json.MarshalIndent(env.History, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(os.TempDir(), ".container-use-git-notes-*")
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(buff); err != nil {
		return err
	}

	_, err = runGitCommand(ctx, s.worktreePath, "notes", "--ref", gitNotesStateRef, "add", "-f", "-F", f.Name())
	return err
}

func (s *storage) addGitNote(ctx context.Context, note string) error {
	_, err := runGitCommand(ctx, s.worktreePath, "notes", "--ref", gitNotesLogRef, "append", "-m", note)
	return err
}

func (s *storage) applyPatch(ctx context.Context, patchContent string) error {
	if strings.TrimSpace(patchContent) == "" {
		return nil
	}

	slog.Info("Applying patch to worktree")

	cmd := exec.Command("git", "apply")
	cmd.Dir = s.worktreePath
	cmd.Stdin = strings.NewReader(patchContent)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to apply patch: %w\nOutput: %s", err, string(output))
	}

	return s.commitChanges(ctx, "Apply patch", "Applied patch with uncommitted changes")
}

func addNonBinaryFiles(ctx context.Context, worktreePath string) error {
	statusOutput, err := runGitCommand(ctx, worktreePath, "status", "--porcelain")
	if err != nil {
		return err
	}

	lines := strings.Split(strings.TrimSpace(statusOutput), "\n")

	for _, line := range lines {
		if line == "" {
			continue
		}
		if len(line) < 3 {
			continue
		}

		indexStatus := line[0]
		workTreeStatus := line[1]
		fileName := strings.TrimSpace(line[2:])
		if fileName == "" {
			continue
		}

		if shouldSkipFile(fileName) {
			continue
		}

		switch {
		case indexStatus == '?' && workTreeStatus == '?':
			if strings.HasSuffix(fileName, "/") {
				dirName := strings.TrimSuffix(fileName, "/")
				if err := addFilesFromUntrackedDirectory(ctx, worktreePath, dirName); err != nil {
					return err
				}
			} else {
				if !isBinaryFile(worktreePath, fileName) {
					_, err = runGitCommand(ctx, worktreePath, "add", fileName)
					if err != nil {
						return err
					}
				}
			}
		case indexStatus == 'A':
			continue
		case indexStatus == 'D' || workTreeStatus == 'D':
			_, err = runGitCommand(ctx, worktreePath, "add", fileName)
			if err != nil {
				return err
			}
		default:
			if !isBinaryFile(worktreePath, fileName) {
				_, err = runGitCommand(ctx, worktreePath, "add", fileName)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func shouldSkipFile(fileName string) bool {
	skipExtensions := []string{
		".tar", ".tar.gz", ".tgz", ".tar.bz2", ".tbz2", ".tar.xz", ".txz",
		".zip", ".rar", ".7z", ".gz", ".bz2", ".xz",
		".exe", ".bin", ".dmg", ".pkg", ".msi",
		".jpg", ".jpeg", ".png", ".gif", ".bmp", ".tiff", ".svg",
		".mp3", ".mp4", ".avi", ".mov", ".wmv", ".flv", ".mkv",
		".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx",
		".so", ".dylib", ".dll", ".a", ".lib",
	}

	lowerName := strings.ToLower(fileName)
	for _, ext := range skipExtensions {
		if strings.HasSuffix(lowerName, ext) {
			return true
		}
	}

	skipPatterns := []string{
		"node_modules/", ".git/", "__pycache__/", ".DS_Store",
		"venv/", ".venv/", "env/", ".env/",
		"target/", "build/", "dist/", ".next/",
		"*.tmp", "*.temp", "*.cache", "*.log",
	}

	for _, pattern := range skipPatterns {
		if strings.Contains(lowerName, strings.ToLower(pattern)) {
			return true
		}
	}

	return false
}

func addFilesFromUntrackedDirectory(ctx context.Context, worktreePath, dirName string) error {
	dirPath := filepath.Join(worktreePath, dirName)

	return filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(worktreePath, path)
		if err != nil {
			return err
		}

		if info.IsDir() {
			if shouldSkipFile(relPath + "/") {
				return filepath.SkipDir
			}
			return nil
		}

		if shouldSkipFile(relPath) {
			return nil
		}

		if !isBinaryFile(worktreePath, relPath) {
			_, err = runGitCommand(ctx, worktreePath, "add", relPath)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

func isBinaryFile(worktreePath, fileName string) bool {
	fullPath := filepath.Join(worktreePath, fileName)

	stat, err := os.Stat(fullPath)
	if err != nil {
		return true
	}

	if stat.IsDir() {
		return false
	}

	if stat.Size() > maxFileSizeForTextCheck {
		return true
	}

	file, err := os.Open(fullPath)
	if err != nil {
		slog.Error("Error opening file", "err", err)
		return true
	}
	defer file.Close()

	buffer := make([]byte, 8000)
	n, err := file.Read(buffer)
	if err != nil && n == 0 {
		return true
	}

	buffer = buffer[:n]
	if slices.Contains(buffer, 0) {
		return true
	}

	return false
}
