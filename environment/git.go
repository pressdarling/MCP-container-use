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
	"strings"

	"github.com/mitchellh/go-homedir"
)

const (
	containerUseRemote = "container-use"
	gitNotesLogRef     = "container-use"
	gitNotesStateRef   = "container-use-state"
)

func (env *Environment) SetupTrackingBranch(ctx context.Context, localRepoPath string) error {
	localRepoPath, err := filepath.Abs(localRepoPath)
	if err != nil {
		return err
	}

	if err := env.createTrackingBranch(ctx, localRepoPath); err != nil {
		return err
	}

	return nil
}

func (env *Environment) createTrackingBranch(ctx context.Context, localRepoPath string) error {
	slog.Info("Setting up tracking branch", "environment", env.ID, "branch", env.ID)

	// Push current branch to storage to establish the tracking branch
	currentBranch, err := runGitCommand(ctx, localRepoPath, "branch", "--show-current")
	if err != nil {
		return err
	}

	currentBranch = strings.TrimSpace(currentBranch)
	if currentBranch == "" {
		return fmt.Errorf("no current branch found")
	}

	// Create the tracking branch by pushing to storage
	_, err = runGitCommand(ctx, localRepoPath, "push", containerUseRemote, fmt.Sprintf("%s:%s", currentBranch, env.ID))
	if err != nil {
		return err
	}

	// Set up the tracking branch locally
	_, err = runGitCommand(ctx, localRepoPath, "branch", "--track", env.ID, fmt.Sprintf("%s/%s", containerUseRemote, env.ID))
	if err != nil {
		return err
	}

	return nil
}

func (env *Environment) PropagateToTrackedBranch(ctx context.Context, name, explanation string) error {
	slog.Info("Propagating to tracked environment branch", "environment", env.ID, "branch", env.ID)

	localRepoPath, err := filepath.Abs(env.source)
	if err != nil {
		return err
	}

	if err := storage.Save(env, name, explanation); err != nil {
		return err
	}

	if err := env.fetchStorage(ctx, localRepoPath); err != nil {
		return err
	}

	// Propagate both state and log notes to source repo
	if err := env.fetchGitNotes(ctx, gitNotesStateRef); err != nil {
		return err
	}

	if err := env.fetchGitNotes(ctx, gitNotesLogRef); err != nil {
		return err
	}

	return nil
}

func (env *Environment) DeleteTrackingBranch() error {
	slog.Info("Deleting tracking branch", "environment", env.ID, "branch", env.ID)

	localRepoPath, err := filepath.Abs(env.source)
	if err != nil {
		return err
	}

	ctx := context.Background()

	// Delete the remote tracking branch
	_, err = runGitCommand(ctx, localRepoPath, "push", containerUseRemote, "--delete", env.ID)
	if err != nil {
		slog.Warn("Failed to delete remote tracking branch", "err", err)
	}

	// Delete local tracking branch
	_, err = runGitCommand(ctx, localRepoPath, "branch", "-D", env.ID)
	if err != nil {
		slog.Warn("Failed to delete local tracking branch", "err", err)
	}

	return env.deleteStorage()
}

// this is violently breaking the remote abstraction right now
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

	// Delete the storage repository ()
	repoPath := storage.RemoteUrl(filepath.Base(env.source))
	if strings.HasPrefix(repoPath, "file://") {
		repoPath = repoPath[7:] // Remove file:// prefix
		slog.Info("Deleting storage repository", "path", repoPath)
		if err := os.RemoveAll(repoPath); err != nil {
			return err
		}
	}

	return nil
}

func (env *Environment) fetchGitNotes(ctx context.Context, ref string) error {
	fullRef := fmt.Sprintf("refs/notes/%s", ref)
	fetch := func() error {
		_, err := runGitCommand(ctx, env.source, "fetch", containerUseRemote, fullRef+":"+fullRef)
		return err
	}

	if err := fetch(); err != nil {
		if strings.Contains(err.Error(), "[rejected]") {
			if _, err := runGitCommand(ctx, env.source, "update-ref", "-d", fullRef); err == nil {
				return fetch()
			}
		}
		return err
	}
	return nil
}

func (env *Environment) addGitNote(ctx context.Context, note string) error {
	return storage.Note(env, note)
}

func StateFromCommit(ctx context.Context, localRepoPath, commitHash string) (History, error) {
	notes, err := runGitCommand(ctx, localRepoPath, "notes", "--ref", gitNotesStateRef, "show", commitHash)
	if err != nil {
		return nil, err
	}

	var history History
	if err := json.Unmarshal([]byte(notes), &history); err != nil {
		return nil, err
	}

	return history, nil
}

func (env *Environment) loadStateFromNotes(ctx context.Context, localRepoPath string) error {
	history, err := StateFromCommit(ctx, localRepoPath, "HEAD")
	if err != nil {
		return err
	}
	env.History = history
	return nil
}

func (env *Environment) GeneratePatch(ctx context.Context) (string, error) {
	localRepoPath, err := filepath.Abs(env.source)
	if err != nil {
		return "", err
	}

	status, err := runGitCommand(ctx, localRepoPath, "status", "--porcelain")
	if err != nil {
		return "", err
	}

	slog.Debug("Git status output", "status", status)

	if strings.TrimSpace(status) == "" {
		slog.Debug("No changes detected, returning empty patch")
		return "", nil
	}

	slog.Info("Generating patch from uncommitted changes")

	// Check if repository has any commits
	_, err = runGitCommand(ctx, localRepoPath, "rev-parse", "HEAD")
	if err != nil {
		slog.Debug("No HEAD commit found, skipping diff generation")
		return "", nil
	}

	// Generate diff patch for tracked changes
	patch, err := runGitCommand(ctx, localRepoPath, "diff", "HEAD")
	if err != nil {
		return "", err
	}

	slog.Debug("Generated diff patch", "patch_length", len(patch))
	return patch, nil
}

func (env *Environment) fetchStorage(ctx context.Context, localRepoPath string) error {
	slog.Info("Fetching tracking branch from storage to source repository")
	_, err := runGitCommand(ctx, localRepoPath, "fetch", containerUseRemote, env.ID)
	return err
}

func (env *Environment) GetWorktreePath() (string, error) {
	return homedir.Expand(fmt.Sprintf("~/.config/container-use/worktrees/%s", env.ID))
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
