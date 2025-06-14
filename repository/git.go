package repository

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dagger/container-use/environment"
)

// 10MB
const maxFileSizeForTextCheck = 10 * 1024 * 1024

func (r *Repository) DeleteWorktree(id string) error {
	worktreePath, err := worktreePath(id)
	if err != nil {
		return err
	}
	parentDir := filepath.Dir(worktreePath)
	fmt.Printf("Deleting parent directory of worktree at %s\n", parentDir)
	return os.RemoveAll(parentDir)
}

func (r *Repository) DeleteLocalRemoteBranch(id string) error {
	slog.Info("Pruning git worktrees", "repo", r.forkRepoPath)
	if _, err := runGitCommand(context.Background(), r.forkRepoPath, "worktree", "prune"); err != nil {
		slog.Error("Failed to prune git worktrees", "repo", r.forkRepoPath, "err", err)
		return err
	}

	slog.Info("Deleting local branch", "repo", r.forkRepoPath, "branch", id)
	if _, err := runGitCommand(context.Background(), r.forkRepoPath, "branch", "-D", id); err != nil {
		slog.Error("Failed to delete local branch", "repo", r.forkRepoPath, "branch", id, "err", err)
		return err
	}

	if _, err := runGitCommand(context.Background(), r.userRepoPath, "remote", "prune", containerUseRemote); err != nil {
		slog.Error("Failed to fetch and prune container-use remote", "local-repo", r.userRepoPath, "err", err)
		return err
	}

	return nil
}

func (r *Repository) initializeWorktree(ctx context.Context, id string) (string, error) {
	worktreePath, err := worktreePath(id)
	if err != nil {
		return "", err
	}

	if _, err := os.Stat(worktreePath); err == nil {
		return worktreePath, nil
	}

	// slog.Info("Initializing worktree", "container-id", id, "container-name", name, "id", id)
	_, err = runGitCommand(ctx, r.userRepoPath, "fetch", containerUseRemote)
	if err != nil {
		return "", err
	}

	currentBranch, err := runGitCommand(ctx, r.userRepoPath, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	currentBranch = strings.TrimSpace(currentBranch)

	// this is racy, i think? like if a human is rewriting history on a branch and creating containers, things get complicated.
	// there's only 1 copy of the source branch in the localremote, so there's potential for conflicts.
	_, err = runGitCommand(ctx, r.userRepoPath, "push", containerUseRemote, "--force", currentBranch)
	if err != nil {
		return "", err
	}

	// create worktree, accomodating past partial failures where the branch pushed but the worktree wasn't created
	_, err = runGitCommand(ctx, r.forkRepoPath, "show-ref", "--verify", "--quiet", fmt.Sprintf("refs/heads/%s", id))
	if err != nil {
		_, err = runGitCommand(ctx, r.forkRepoPath, "worktree", "add", "-b", id, worktreePath, currentBranch)
		if err != nil {
			return "", err
		}
	} else {
		_, err = runGitCommand(ctx, r.forkRepoPath, "worktree", "add", worktreePath, id)
		if err != nil {
			return "", err
		}
	}

	if err := r.applyUncommittedChanges(ctx, worktreePath); err != nil {
		return "", fmt.Errorf("failed to apply uncommitted changes: %w", err)
	}

	_, err = runGitCommand(ctx, r.userRepoPath, "fetch", containerUseRemote, id)
	if err != nil {
		return "", err
	}

	// set up remote tracking branch if it's not already there
	_, err = runGitCommand(ctx, r.userRepoPath, "show-ref", "--verify", "--quiet", fmt.Sprintf("refs/heads/%s", id))
	if err != nil {
		_, err = runGitCommand(ctx, r.userRepoPath, "branch", "--track", id, fmt.Sprintf("%s/%s", containerUseRemote, id))
		if err != nil {
			return "", err
		}
	}

	return worktreePath, nil
}

func (r *Repository) propagateToWorktree(ctx context.Context, env *environment.Environment, name, explanation string) (rerr error) {
	slog.Info("Propagating to worktree...",
		"environment.id", env.ID,
		"environment.name", env.Name,
		"workdir", env.Config.Workdir,
		"id", env.ID)
	defer func() {
		slog.Info("Propagating to worktree... (DONE)",
			"environment.id", env.ID,
			"environment.name", env.Name,
			"workdir", env.Config.Workdir,
			"id", env.ID,
			"err", rerr)
	}()

	if err := env.Export(ctx); err != nil {
		return err
	}

	if err := r.commitWorktreeChanges(ctx, env.Worktree, name, explanation); err != nil {
		return fmt.Errorf("failed to commit worktree changes: %w", err)
	}

	if err := r.saveState(ctx, env); err != nil {
		return fmt.Errorf("failed to add notes: %w", err)
	}

	slog.Info("Fetching container-use remote in source repository")
	if _, err := runGitCommand(ctx, r.userRepoPath, "fetch", containerUseRemote, env.ID); err != nil {
		return err
	}

	if err := r.propagateGitNotes(ctx, gitNotesStateRef); err != nil {
		return err
	}

	return nil
}

func (r *Repository) propagateGitNotes(ctx context.Context, ref string) error {
	fullRef := fmt.Sprintf("refs/notes/%s", ref)
	fetch := func() error {
		_, err := runGitCommand(ctx, r.userRepoPath, "fetch", containerUseRemote, fullRef+":"+fullRef)
		return err
	}

	if err := fetch(); err != nil {
		if strings.Contains(err.Error(), "[rejected]") {
			if _, err := runGitCommand(ctx, r.userRepoPath, "update-ref", "-d", fullRef); err == nil {
				return fetch()
			}
		}
		return err
	}
	return nil
}

func (r *Repository) saveState(ctx context.Context, env *environment.Environment) error {
	state, err := env.State(ctx)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(os.TempDir(), ".container-use-git-notes-*")
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(state); err != nil {
		return err
	}

	_, err = runGitCommand(ctx, env.Worktree, "notes", "--ref", gitNotesStateRef, "add", "-f", "-F", f.Name())
	if err != nil {
		return err
	}
	return nil
}

func (r *Repository) loadState(ctx context.Context, worktreePath string) ([]byte, error) {
	buff, err := runGitCommand(ctx, worktreePath, "notes", "--ref", gitNotesStateRef, "show")
	if err != nil {
		if strings.Contains(err.Error(), "no note found") {
			return nil, nil
		}
		return nil, err
	}
	return []byte(buff), nil
}

func (r *Repository) addGitNote(ctx context.Context, env *environment.Environment, note string) error {
	_, err := runGitCommand(ctx, env.Worktree, "notes", "--ref", gitNotesLogRef, "append", "-m", note)
	if err != nil {
		return err
	}
	return r.propagateGitNotes(ctx, gitNotesLogRef)
}

func (r *Repository) commitWorktreeChanges(ctx context.Context, worktreePath, name, explanation string) error {
	status, err := runGitCommand(ctx, worktreePath, "status", "--porcelain")
	if err != nil {
		return err
	}

	if strings.TrimSpace(status) == "" {
		return nil
	}

	if err := r.addNonBinaryFiles(ctx, worktreePath); err != nil {
		return err
	}

	commitMsg := fmt.Sprintf("%s\n\n%s", name, explanation)
	_, err = runGitCommand(ctx, worktreePath, "commit", "-m", commitMsg)
	return err
}

// AI slop below!
// this is just to keep us moving fast because big git repos get hard to work with
// and our demos like to download large dependencies.
func (r *Repository) addNonBinaryFiles(ctx context.Context, worktreePath string) error {
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

		if r.shouldSkipFile(fileName) {
			continue
		}

		switch {
		case indexStatus == '?' && workTreeStatus == '?':
			// ?? = untracked files or directories
			if strings.HasSuffix(fileName, "/") {
				// Untracked directory - traverse and add non-binary files
				dirName := strings.TrimSuffix(fileName, "/")
				if err := r.addFilesFromUntrackedDirectory(ctx, worktreePath, dirName); err != nil {
					return err
				}
			} else {
				// Untracked file - add if not binary
				if !r.isBinaryFile(worktreePath, fileName) {
					_, err = runGitCommand(ctx, worktreePath, "add", fileName)
					if err != nil {
						return err
					}
				}
			}
		case indexStatus == 'A':
			// A = already staged, skip
			continue
		case indexStatus == 'D' || workTreeStatus == 'D':
			// D = deleted files (always stage deletion)
			_, err = runGitCommand(ctx, worktreePath, "add", fileName)
			if err != nil {
				return err
			}
		default:
			// M, R, C and other statuses - add if not binary
			if !r.isBinaryFile(worktreePath, fileName) {
				_, err = runGitCommand(ctx, worktreePath, "add", fileName)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (r *Repository) shouldSkipFile(fileName string) bool {
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

func (r *Repository) applyUncommittedChanges(ctx context.Context, worktreePath string) error {
	status, err := runGitCommand(ctx, r.userRepoPath, "status", "--porcelain")
	if err != nil {
		return err
	}

	if strings.TrimSpace(status) == "" {
		return nil
	}

	// slog.Info("Applying uncommitted changes to worktree", "container-id", r.ID, "container-name", r.Name)

	patch, err := runGitCommand(ctx, r.userRepoPath, "diff", "HEAD")
	if err != nil {
		return err
	}

	if strings.TrimSpace(patch) != "" {
		cmd := exec.Command("git", "apply")
		cmd.Dir = worktreePath
		cmd.Stdin = strings.NewReader(patch)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to apply patch: %w", err)
		}
	}

	untrackedFiles, err := runGitCommand(ctx, r.userRepoPath, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return err
	}

	for _, file := range strings.Split(strings.TrimSpace(untrackedFiles), "\n") {
		if file == "" {
			continue
		}
		srcPath := filepath.Join(r.userRepoPath, file)
		destPath := filepath.Join(worktreePath, file)

		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}

		if err := exec.Command("cp", "-r", srcPath, destPath).Run(); err != nil {
			return fmt.Errorf("failed to copy untracked file %s: %w", file, err)
		}
	}

	return r.commitWorktreeChanges(ctx, worktreePath, "Copy uncommitted changes", "Applied uncommitted changes from local repository")
}

func (r *Repository) addFilesFromUntrackedDirectory(ctx context.Context, worktreePath, dirName string) error {
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
			if r.shouldSkipFile(relPath + "/") {
				return filepath.SkipDir
			}
			return nil
		}

		if r.shouldSkipFile(relPath) {
			return nil
		}

		if !r.isBinaryFile(worktreePath, relPath) {
			_, err = runGitCommand(ctx, worktreePath, "add", relPath)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

func (r *Repository) isBinaryFile(worktreePath, fileName string) bool {
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
