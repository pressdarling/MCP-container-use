package environment

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
)

const (
	containerUseRemote = "container-use"
	gitNotesLogRef     = "container-use"
	gitNotesStateRef   = "container-use-state"
)

const maxFileSizeForTextCheck = 10 * 1024 * 1024

func (env *Environment) SetupTrackingBranch(ctx context.Context, localRepoPath string) error {
	localRepoPath, err := filepath.Abs(localRepoPath)
	if err != nil {
		return err
	}

	storage, err := env.initializeStorage(ctx, localRepoPath)
	if err != nil {
		return err
	}

	if err := env.createTrackingBranch(ctx, localRepoPath, storage); err != nil {
		return err
	}

	if err := storage.applyUncommittedChanges(ctx, localRepoPath); err != nil {
		return fmt.Errorf("failed to apply uncommitted changes: %w", err)
	}

	return nil
}

func (env *Environment) createTrackingBranch(ctx context.Context, localRepoPath string, storage *Storage) error {
	slog.Info("Setting up tracking branch", "environment", env.ID, "branch", env.ID)

	// Fetch current complete storage state
	_, err := runGitCommand(ctx, localRepoPath, "fetch", containerUseRemote)
	if err != nil {
		return err
	}

	// Push current branch to storage to establish the tracking branch
	currentBranch, err := runGitCommand(ctx, localRepoPath, "branch", "--show-current")
	if err != nil {
		return err
	}
	currentBranch = strings.TrimSpace(currentBranch)

	_, err = runGitCommand(ctx, localRepoPath, "push", containerUseRemote, "--force", currentBranch)
	if err != nil {
		return err
	}

	// Create storage worktree for this tracking branch
	if err := storage.createWorktree(ctx, currentBranch); err != nil {
		return err
	}

	// Establish the tracking branch in source repo
	_, err = runGitCommand(ctx, localRepoPath, "fetch", containerUseRemote, env.ID)
	if err != nil {
		return err
	}

	_, err = runGitCommand(ctx, localRepoPath, "show-ref", "--verify", "--quiet", fmt.Sprintf("refs/heads/%s", env.ID))
	if err != nil {
		_, err = runGitCommand(ctx, localRepoPath, "branch", "--track", env.ID, fmt.Sprintf("%s/%s", containerUseRemote, env.ID))
		if err != nil {
			return err
		}
	}

	return nil
}

func (env *Environment) PropagateToTrackedBranch(ctx context.Context, name, explanation string) error {
	slog.Info("Propagating to tracked environment branch", "environment", env.ID, "branch", env.ID)

	localRepoPath, err := filepath.Abs(env.Source)
	if err != nil {
		return err
	}

	storage, err := env.initializeStorage(ctx, localRepoPath)
	if err != nil {
		return err
	}

	if err := storage.save(ctx, env); err != nil {
		return err
	}

	// Commit changes in storage layer
	if err := storage.commitChanges(ctx, name, explanation); err != nil {
		return fmt.Errorf("failed to commit to storage: %w", err)
	}

	// Commit state to tracking branch
	if err := storage.commitStateToNotes(ctx, env); err != nil {
		return fmt.Errorf("failed to add notes to tracking branch: %w", err)
	}

	// Fetch tracking branch from storage to source repo (needed for notes propagation)
	if err := env.fetchStorageChanges(ctx, localRepoPath); err != nil {
		return err
	}

	// Propagate both state and log notes to source repo
	if err := env.propagateGitNotes(ctx, gitNotesStateRef); err != nil {
		return err
	}

	if err := env.propagateGitNotes(ctx, gitNotesLogRef); err != nil {
		return err
	}

	return nil
}

func (env *Environment) DeleteTrackingBranch() error {
	// Clean up storage layer
	if err := env.deleteStorage(); err != nil {
		return err
	}

	// Remove tracking branch from source repo
	localRepoPath, err := filepath.Abs(env.Source)
	if err != nil {
		return err
	}

	if _, err = runGitCommand(context.Background(), localRepoPath, "remote", "prune", containerUseRemote); err != nil {
		slog.Error("Failed to prune container-use remote", "repo", localRepoPath, "err", err)
		return err
	}

	return nil
}

// This initial storage impl uses a local file:// remote and worktrees to provide storage to the container-use environment.
// We have yet to define an interface around this, but it should be swappable for remote-remotes in the future, or impls that put container state somewhere other than git notes.
type Storage struct {
	repoPath     string
	worktreePath string
	Branch       string
}

func (env *Environment) initializeStorage(ctx context.Context, localRepoPath string) (*Storage, error) {
	cuRepoPath, err := initializeLocalRemote(ctx, localRepoPath)
	if err != nil {
		return nil, err
	}

	worktreePath, err := homedir.Expand(fmt.Sprintf("~/.config/container-use/worktrees/%s", env.ID))
	if err != nil {
		return nil, err
	}

	return &Storage{
		repoPath:     cuRepoPath,
		worktreePath: worktreePath,
		Branch:       env.ID,
	}, nil
}

func (storage *Storage) createWorktree(ctx context.Context, sourceBranch string) error {
	if _, err := os.Stat(storage.worktreePath); err == nil {
		return nil
	}

	// Create worktree from storage repo
	_, err := runGitCommand(ctx, storage.repoPath, "show-ref", "--verify", "--quiet", fmt.Sprintf("refs/heads/%s", filepath.Base(storage.worktreePath)))
	if err != nil {
		_, err = runGitCommand(ctx, storage.repoPath, "worktree", "add", "-b", storage.Branch, storage.worktreePath, sourceBranch)
		if err != nil {
			return err
		}
	} else {
		_, err = runGitCommand(ctx, storage.repoPath, "worktree", "add", storage.worktreePath, storage.Branch)
		if err != nil {
			return err
		}
	}

	return nil
}

func (storage *Storage) commitChanges(ctx context.Context, name, explanation string) error {
	status, err := runGitCommand(ctx, storage.worktreePath, "status", "--porcelain")
	if err != nil {
		return err
	}

	if strings.TrimSpace(status) == "" {
		return nil
	}

	if err := addNonBinaryFiles(ctx, storage.worktreePath); err != nil {
		return err
	}

	commitMsg := fmt.Sprintf("%s\n\n%s", name, explanation)
	_, err = runGitCommand(ctx, storage.worktreePath, "commit", "-m", commitMsg)
	return err
}

func (storage *Storage) Workdir() *dagger.Directory {
	return dag.Host().Directory(storage.worktreePath)
}

func (storage *Storage) save(ctx context.Context, env *Environment) error {
	_, err := env.container.Directory(env.Workdir).Export(
		ctx,
		storage.worktreePath,
		dagger.DirectoryExportOpts{Wipe: true},
	)
	if err != nil {
		return err
	}

	cfg := filepath.Join(storage.worktreePath, ".container-use")
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

func (storage *Storage) load(env *Environment) error {
	cfg := filepath.Join(storage.worktreePath, ".container-use")

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

func (env *Environment) deleteStorage() error {
	// Delete worktree
	worktreePath, err := env.GetWorktreePath()
	if err != nil {
		return err
	}
	parentDir := filepath.Dir(worktreePath)
	slog.Info("Deleting storage worktree", "path", parentDir)
	if err := os.RemoveAll(parentDir); err != nil {
		return err
	}

	// Clean up storage repo
	localRepoPath, err := filepath.Abs(env.Source)
	if err != nil {
		return err
	}
	repoName := filepath.Base(localRepoPath)
	cuRepoPath, err := getRepoPath(repoName)
	if err != nil {
		return err
	}

	slog.Info("Pruning storage worktrees", "repo", cuRepoPath)
	if _, err = runGitCommand(context.Background(), cuRepoPath, "worktree", "prune"); err != nil {
		slog.Error("Failed to prune worktrees", "repo", cuRepoPath, "err", err)
		return err
	}

	slog.Info("Deleting storage branch", "repo", cuRepoPath, "branch", env.ID)
	if _, err = runGitCommand(context.Background(), cuRepoPath, "branch", "-D", env.ID); err != nil {
		slog.Error("Failed to delete storage branch", "repo", cuRepoPath, "branch", env.ID, "err", err)
		return err
	}

	return nil
}

func getRepoPath(repoName string) (string, error) {
	return homedir.Expand(fmt.Sprintf("~/.config/container-use/repos/%s", filepath.Base(repoName)))
}

func initializeLocalRemote(ctx context.Context, localRepoPath string) (string, error) {
	localRepoPath, err := filepath.Abs(localRepoPath)
	if err != nil {
		return "", err
	}

	repoName := filepath.Base(localRepoPath)
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

func (env *Environment) propagateGitNotes(ctx context.Context, ref string) error {
	fullRef := fmt.Sprintf("refs/notes/%s", ref)
	fetch := func() error {
		_, err := runGitCommand(ctx, env.Source, "fetch", containerUseRemote, fullRef+":"+fullRef)
		return err
	}

	if err := fetch(); err != nil {
		if strings.Contains(err.Error(), "[rejected]") {
			if _, err := runGitCommand(ctx, env.Source, "update-ref", "-d", fullRef); err == nil {
				return fetch()
			}
		}
		return err
	}
	return nil
}

func (storage *Storage) commitStateToNotes(ctx context.Context, env *Environment) error {
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

	_, err = runGitCommand(ctx, storage.worktreePath, "notes", "--ref", gitNotesStateRef, "add", "-f", "-F", f.Name())
	return err
}

func (storage *Storage) addGitNote(ctx context.Context, note string) error {
	_, err := runGitCommand(ctx, storage.worktreePath, "notes", "--ref", gitNotesLogRef, "append", "-m", note)
	return err
}

func (env *Environment) addGitNote(ctx context.Context, note string) error {
	localRepoPath, err := filepath.Abs(env.Source)
	if err != nil {
		return err
	}

	storage, err := env.initializeStorage(ctx, localRepoPath)
	if err != nil {
		return err
	}

	if err := storage.addGitNote(ctx, note); err != nil {
		return err
	}
	return env.propagateGitNotes(ctx, gitNotesLogRef)
}

func StateFromCommit(ctx context.Context, repoDir, commit string) (History, error) {
	buff, err := runGitCommand(ctx, repoDir, "notes", "--ref", gitNotesStateRef, "show")
	if err != nil {
		return nil, err
	}

	var history History
	if err := json.Unmarshal([]byte(buff), &history); err != nil {
		return nil, err
	}
	return history, nil
}

func (env *Environment) loadStateFromNotes(ctx context.Context, worktreePath string) error {
	buff, err := runGitCommand(ctx, worktreePath, "notes", "--ref", gitNotesStateRef, "show")
	if err != nil {
		if strings.Contains(err.Error(), "no note found") {
			return nil
		}
		return err
	}
	return json.Unmarshal([]byte(buff), &env.History)
}

func (env *Environment) fetchStorageChanges(ctx context.Context, localRepoPath string) error {
	slog.Info("Fetching tracking branch from storage to source repository")
	_, err := runGitCommand(ctx, localRepoPath, "fetch", containerUseRemote, env.ID)
	return err
}

func (storage *Storage) applyUncommittedChanges(ctx context.Context, localRepoPath string) error {
	status, err := runGitCommand(ctx, localRepoPath, "status", "--porcelain")
	if err != nil {
		return err
	}

	if strings.TrimSpace(status) == "" {
		return nil
	}

	slog.Info("Applying uncommitted changes to tracked branch")

	patch, err := runGitCommand(ctx, localRepoPath, "diff", "HEAD")
	if err != nil {
		return err
	}

	if strings.TrimSpace(patch) != "" {
		cmd := exec.Command("git", "apply")
		cmd.Dir = storage.worktreePath
		cmd.Stdin = strings.NewReader(patch)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to apply patch: %w", err)
		}
	}

	untrackedFiles, err := runGitCommand(ctx, localRepoPath, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return err
	}

	for _, file := range strings.Split(strings.TrimSpace(untrackedFiles), "\n") {
		if file == "" {
			continue
		}
		srcPath := filepath.Join(localRepoPath, file)
		destPath := filepath.Join(storage.worktreePath, file)

		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}

		if err := exec.Command("cp", "-r", srcPath, destPath).Run(); err != nil {
			return fmt.Errorf("failed to copy untracked file %s: %w", file, err)
		}
	}

	return storage.commitChanges(ctx, "Copy uncommitted changes", "Applied uncommitted changes from local repository")
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

// Backward compatibility methods
func (env *Environment) GetWorktreePath() (string, error) {
	return homedir.Expand(fmt.Sprintf("~/.config/container-use/worktrees/%s", env.ID))
}
