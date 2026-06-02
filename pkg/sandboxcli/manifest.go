package sandboxcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v2"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"github.com/xiaods/k8e/pkg/sandbox/client"
)

// gitRepoRe validates git repo URLs: scheme://host/path or git@host:path
var gitRepoRe = regexp.MustCompile(`^(https?://[\w.-]+(/[\w./:~?#\[\]@!$&'()*+,;=%-]*)?|git@[\w.-]+:[\w./:~?#\[\]@!$&'()*+,;=%-]+)$`)

// gitRefRe validates branch/tag names (no shell metacharacters)
var gitRefRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]*$`)

// ManifestEntry represents one entry in a workspace manifest.
type ManifestEntry struct {
	File    *FileEntry    `yaml:"file,omitempty"`
	Dir     *DirEntry     `yaml:"dir,omitempty"`
	GitRepo *GitRepoEntry `yaml:"gitRepo,omitempty"`
}

// FileEntry declares a file to be written into the sandbox.
type FileEntry struct {
	Path    string `yaml:"path"`
	Content string `yaml:"content"`
	Mode    string `yaml:"mode,omitempty"` // "0755" etc.
}

// DirEntry declares a directory to be created.
type DirEntry struct {
	Path string `yaml:"path"`
}

// GitRepoEntry declares a git repository to be cloned.
type GitRepoEntry struct {
	Path string `yaml:"path"`
	Repo string `yaml:"repo"`
	Ref  string `yaml:"ref,omitempty"` // default: main
}

// Manifest is the top-level structure of a workspace manifest YAML file.
type Manifest struct {
	Entries []ManifestEntry `yaml:"entries"`
}

// parseManifest reads and parses a manifest YAML file.
func parseManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	if len(m.Entries) == 0 {
		return &m, nil
	}
	return &m, nil
}

// materializeManifest applies all entries from a parsed manifest into a sandbox session.
// On error, returns the 1-based index of the failed entry.
func materializeManifest(client *client.Client, sid string, m *Manifest) error {
	if m == nil || len(m.Entries) == 0 {
		return nil
	}
	ctx := context.Background()

	for i, entry := range m.Entries {
		switch {
		case entry.File != nil:
			if err := materializeFile(ctx, client, sid, entry.File); err != nil {
				return fmt.Errorf("entry %d/%d (file %s): %w", i+1, len(m.Entries), entry.File.Path, err)
			}
		case entry.Dir != nil:
			if err := materializeDir(ctx, client, sid, entry.Dir); err != nil {
				return fmt.Errorf("entry %d/%d (dir %s): %w", i+1, len(m.Entries), entry.Dir.Path, err)
			}
		case entry.GitRepo != nil:
			if err := materializeGitRepo(ctx, client, sid, entry.GitRepo); err != nil {
				return fmt.Errorf("entry %d/%d (gitRepo %s): %w", i+1, len(m.Entries), entry.GitRepo.Path, err)
			}
		default:
			return fmt.Errorf("entry %d/%d: no valid type (file, dir, or gitRepo)", i+1, len(m.Entries))
		}
		fmt.Fprintf(os.Stderr, "[k8e-sandbox] ✓ %s\n", entryDesc(entry))
	}
	return nil
}

func materializeFile(ctx context.Context, client *client.Client, sid string, f *FileEntry) error {
	mode := "w"
	if f.Mode != "" {
		mode = f.Mode
	}
	_, err := client.SandboxServiceClient.WriteFile(ctx, &pb.WriteFileRequest{
		SessionId: sid, Path: filepath.Join("/workspace", f.Path), Content: f.Content, Mode: mode,
	})
	return err
}

func materializeDir(ctx context.Context, client *client.Client, sid string, d *DirEntry) error {
	_, err := client.SandboxServiceClient.Exec(ctx, &pb.ExecRequest{
		SessionId: sid, Command: "mkdir -p " + filepath.Join("/workspace", d.Path), Timeout: 10,
	})
	return err
}

func materializeGitRepo(ctx context.Context, client *client.Client, sid string, g *GitRepoEntry) error {
	if err := validateGitRepo(g); err != nil {
		return err
	}
	ref := g.Ref
	if ref == "" {
		ref = "main"
	}
	cmd := fmt.Sprintf("git clone --depth 1 -b %s -- %s %s", ref, g.Repo, filepath.Join("/workspace", g.Path))
	_, err := client.SandboxServiceClient.Exec(ctx, &pb.ExecRequest{
		SessionId: sid, Command: cmd, Timeout: 120,
	})
	return err
}

// validateGitRepo checks that repo URL and ref contain no shell metacharacters.
func validateGitRepo(g *GitRepoEntry) error {
	if !gitRepoRe.MatchString(g.Repo) {
		return fmt.Errorf("invalid git repo URL: %q", g.Repo)
	}
	if g.Ref != "" && !gitRefRe.MatchString(g.Ref) {
		return fmt.Errorf("invalid git ref: %q", g.Ref)
	}
	return nil
}

func entryDesc(e ManifestEntry) string {
	switch {
	case e.File != nil:
		return fmt.Sprintf("file %s", e.File.Path)
	case e.Dir != nil:
		return fmt.Sprintf("dir %s/", e.Dir.Path)
	case e.GitRepo != nil:
		return fmt.Sprintf("gitRepo %s → %s", e.GitRepo.Repo, e.GitRepo.Path)
	}
	return "unknown"
}
