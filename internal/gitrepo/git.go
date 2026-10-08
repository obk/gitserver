package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"go-git-server/internal/render"
)

const (
	Timeout      = 30 * time.Second
	maxGitOutput = 16 << 20
	MaxBlobView  = 1 << 20
	maxDiffView  = 2 << 20
)

var (
	errNotFound  = errors.New("not found")
	errTooLarge  = errors.New("git output too large")
	HashRe       = regexp.MustCompile(`^[0-9a-f]{4,64}$`)
	shortstatRe  = regexp.MustCompile(`(\d+) files? changed(?:, (\d+) insertions?\(\+\))?(?:, (\d+) deletions?\(-\))?`)
	specialFiles = map[string]string{
		"readme": "readme", "readme.md": "readme", "readme.txt": "readme", "readme.rst": "readme", "readme.markdown": "readme",
		"license": "license", "license.md": "license", "license.txt": "license", "copying": "license", "licence": "license",
	}
)

// gitSlots limits concurrent git processes started by the web UI, so a
// burst of requests cannot exhaust the CPU.
var gitSlots = make(chan struct{}, max(4, 2*runtime.NumCPU()))

var errBusy503 = errors.New("server busy")

func AcquireGit(ctx context.Context) (release func(), err error) {
	select {
	case gitSlots <- struct{}{}:
		return func() { <-gitSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(Timeout):
		return nil, errBusy503
	}
}

func Command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	return cmd
}

// runGit returns at most limit bytes of stdout. If the output is longer, the
// process is killed and truncated is set.
func runGit(ctx context.Context, dir string, limit int64, args ...string) (out []byte, truncated bool, err error) {
	release, err := AcquireGit(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmd := Command(ctx, dir, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, limit+1))
	if int64(len(out)) > limit {
		cancel()
		cmd.Wait()
		return out[:limit], true, nil
	}
	if err := cmd.Wait(); err != nil {
		return out, false, fmt.Errorf("git %s: %w: %s", args[0], err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, false, readErr
}

func gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	out, truncated, err := runGit(ctx, dir, maxGitOutput, args...)
	if truncated {
		return nil, errTooLarge
	}
	return out, err
}

// validRev accepts branch, tag and hash names but nothing that git could
// parse as an option or a rev:path expression.
func validRev(rev string) bool {
	if rev == "" || len(rev) > 256 || rev[0] == '-' || strings.Contains(rev, "..") {
		return false
	}
	for _, c := range rev {
		if c <= ' ' || c == 0x7f || strings.ContainsRune(":\\?*[~^", c) {
			return false
		}
	}
	return true
}

// ResolveCommit turns rev (or HEAD, or the newest branch when HEAD is
// unborn) into a full commit hash.
func ResolveCommit(ctx context.Context, dir, rev string) (string, error) {
	if rev == "" {
		if h, err := ResolveCommit(ctx, dir, "HEAD"); err == nil {
			return h, nil
		}
		out, err := gitOutput(ctx, dir, "for-each-ref", "--count=1", "--sort=-committerdate", "--format=%(objectname)", "refs/heads")
		if err != nil || len(bytes.TrimSpace(out)) == 0 {
			return "", errNotFound
		}
		return strings.TrimSpace(string(out)), nil
	}
	if !validRev(rev) {
		return "", errNotFound
	}
	out, err := gitOutput(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", errNotFound
	}
	hash := strings.TrimSpace(string(out))
	if !reachable(ctx, dir, hash) {
		return "", errNotFound
	}
	return hash, nil
}

// reachable reports whether commit is on a branch or tag. Commits left
// behind by a force push or a deleted branch stay in the repository until
// git gc prunes them, and may hold what the owner meant to remove.
func reachable(ctx context.Context, dir, commit string) bool {
	if !HashRe.MatchString(commit) {
		return false
	}
	// Prints commit unless it is reachable from refs/heads or refs/tags.
	out, err := gitOutput(ctx, dir, "rev-list", "-n", "1", commit, "--not", "--branches", "--tags", "--")
	return err == nil && len(bytes.TrimSpace(out)) == 0
}

func LastChange(ctx context.Context, dir string) time.Time {
	out, err := gitOutput(ctx, dir, "for-each-ref", "--count=1", "--sort=-committerdate", "--format=%(committerdate:unix)", "refs/heads")
	if err != nil {
		return time.Time{}
	}
	return parseUnix(strings.TrimSpace(string(out)))
}

func parseUnix(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

type Commit struct {
	Hash, Author, Subject string
	Body                  string // only from Messages
	Date                  time.Time
	Files, Add, Del       int
}

// Messages lists the newest n commits reachable from commit with their
// full messages, for feeds. No diff stats.
func Messages(ctx context.Context, dir, commit string, n int) ([]Commit, error) {
	out, err := gitOutput(ctx, dir, "log", "--no-color", "--format=%x1e%H%x00%an%x00%at%x00%s%x00%b", "-n", strconv.Itoa(n), commit, "--")
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for _, rec := range strings.Split(string(out), "\x1e") {
		f := strings.SplitN(rec, "\x00", 5)
		if len(f) != 5 {
			continue
		}
		commits = append(commits, Commit{Hash: f[0], Author: f[1], Date: parseUnix(f[2]), Subject: f[3], Body: strings.TrimSpace(f[4])})
	}
	return commits, nil
}

// Log lists commits reachable from commit, optionally only those touching path.
func Log(ctx context.Context, dir, commit, path string, skip, n int) ([]Commit, error) {
	args := []string{"log", "--no-color", "--no-ext-diff", "--no-textconv", "--format=%x1e%H%x00%an%x00%at%x00%s", "--shortstat",
		"--skip=" + strconv.Itoa(skip), "-n", strconv.Itoa(n), commit, "--"}
	if path != "" {
		args = append(args, ":(literal)"+path)
	}
	out, err := gitOutput(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for _, rec := range strings.Split(string(out), "\x1e") {
		if rec == "" {
			continue
		}
		head, stat, _ := strings.Cut(rec, "\n")
		f := strings.Split(head, "\x00")
		if len(f) != 4 {
			continue
		}
		c := Commit{Hash: f[0], Author: f[1], Date: parseUnix(f[2]), Subject: f[3]}
		if m := shortstatRe.FindStringSubmatch(stat); m != nil {
			c.Files, _ = strconv.Atoi(m[1])
			c.Add, _ = strconv.Atoi(m[2])
			c.Del, _ = strconv.Atoi(m[3])
		}
		commits = append(commits, c)
	}
	return commits, nil
}

type TreeEntry struct {
	Mode, Type, Name string
	Size             int64
}

func ObjectType(ctx context.Context, dir, commit, path string) (string, error) {
	out, err := gitOutput(ctx, dir, "cat-file", "-t", "--end-of-options", commit+":"+path)
	if err != nil {
		return "", errNotFound
	}
	return strings.TrimSpace(string(out)), nil
}

func Tree(ctx context.Context, dir, commit, path string) ([]TreeEntry, error) {
	out, err := gitOutput(ctx, dir, "ls-tree", "-z", "-l", "--end-of-options", commit+":"+path)
	if err != nil {
		return nil, errNotFound
	}
	var entries []TreeEntry
	for _, rec := range strings.Split(string(out), "\x00") {
		meta, name, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 4 {
			continue
		}
		size, _ := strconv.ParseInt(f[3], 10, 64)
		entries = append(entries, TreeEntry{Mode: f[0], Type: f[1], Name: name, Size: size})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return (entries[i].Type == "tree") && (entries[j].Type != "tree")
	})
	return entries, nil
}

func BlobSize(ctx context.Context, dir, commit, path string) (int64, error) {
	out, err := gitOutput(ctx, dir, "cat-file", "-s", "--end-of-options", commit+":"+path)
	if err != nil {
		return 0, errNotFound
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

func BlobContent(ctx context.Context, dir, commit, path string) ([]byte, error) {
	return gitOutput(ctx, dir, "cat-file", "blob", commit+":"+path)
}

// IsReadme reports whether a file name is a README (README, README.md, ...).
func IsReadme(name string) bool { return specialFiles[strings.ToLower(name)] == "readme" }

// SpecialFilesAt finds README and LICENSE in the root tree of commit.
func SpecialFilesAt(ctx context.Context, dir, commit string) (readme, license string) {
	out, err := gitOutput(ctx, dir, "ls-tree", "-z", "--name-only", "--end-of-options", commit)
	if err != nil {
		return "", ""
	}
	for _, name := range strings.Split(string(out), "\x00") {
		switch specialFiles[strings.ToLower(name)] {
		case "readme":
			if readme == "" {
				readme = name
			}
		case "license":
			if license == "" {
				license = name
			}
		}
	}
	return readme, license
}

type CommitInfo struct {
	Hash      string
	Parents   []string
	Author    string
	Email     string
	Date      time.Time
	Message   string
	Stat      string
	Diff      []render.DiffLine
	Truncated bool
}

func ReadCommit(ctx context.Context, dir, hash string) (*CommitInfo, error) {
	out, err := gitOutput(ctx, dir, "show", "-s", "--no-color", "--format=%H%x00%P%x00%an%x00%ae%x00%at%x00%B", hash, "--")
	if err != nil {
		return nil, errNotFound
	}
	f := strings.SplitN(string(out), "\x00", 6)
	if len(f) != 6 {
		return nil, errNotFound
	}
	c := &CommitInfo{Hash: f[0], Parents: strings.Fields(f[1]), Author: f[2], Email: f[3], Date: parseUnix(f[4]),
		Message: strings.TrimRight(f[5], "\n")}

	// --no-ext-diff/--no-textconv: never run programs named in git config
	// or selected through .gitattributes in the repository.
	diffArgs := []string{"show", "--no-color", "--no-ext-diff", "--no-textconv", "--format=", "-M", "--diff-merges=first-parent"}
	stat, _, err := runGit(ctx, dir, 256<<10, append(diffArgs, "--stat=120,80", hash, "--")...)
	if err != nil {
		return nil, err
	}
	c.Stat = strings.TrimRight(string(stat), "\n")
	patch, truncated, err := runGit(ctx, dir, maxDiffView, append(diffArgs, "--patch", hash, "--")...)
	if err != nil {
		return nil, err
	}
	c.Truncated = truncated
	c.Diff = render.Diff(string(patch), len(patch) <= 1<<20)
	return c, nil
}

type Ref struct {
	Name, Full, Author string
	Subject            string // of the tag message, or of the commit for a plain tag
	Date               time.Time
}

func Refs(ctx context.Context, dir string) (branches, tags []Ref, err error) {
	out, err := gitOutput(ctx, dir, "for-each-ref", "--sort=-creatordate",
		"--format=%(refname)%00%(creatordate:unix)%00%(authorname)%(taggername)%00%(contents:subject)", "refs/heads", "refs/tags")
	if err != nil {
		return nil, nil, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(line, "\x00")
		if len(f) != 4 {
			continue
		}
		r := Ref{Full: f[0], Date: parseUnix(f[1]), Author: f[2], Subject: f[3]}
		if name, ok := strings.CutPrefix(f[0], "refs/heads/"); ok {
			r.Name = name
			branches = append(branches, r)
		} else if name, ok := strings.CutPrefix(f[0], "refs/tags/"); ok {
			r.Name = name
			tags = append(tags, r)
		}
	}
	return branches, tags, nil
}

// A GrepMatch is one line that Grep found.
type GrepMatch struct {
	Path string
	Line int
	Text string
}

const (
	maxGrepPerFile = 20
	maxGrepOutput  = 1 << 20
)

// Grep searches the files of commit for the fixed string query (in any
// case if ignoreCase), skipping binary files, and returns up to max
// matching lines, at most maxGrepPerFile per file. truncated says there
// were more.
func Grep(ctx context.Context, dir, commit, query string, ignoreCase bool, max int) (matches []GrepMatch, truncated bool, err error) {
	if !HashRe.MatchString(commit) || query == "" || strings.ContainsAny(query, "\x00\n") {
		return nil, false, errNotFound
	}
	// Not --max-count: it needs git 2.38.
	args := []string{"grep", "--no-color", "-I", "-n", "-z", "--full-name", "-F"}
	if ignoreCase {
		args = append(args, "-i")
	}
	args = append(args, "-e", query, commit, "--")
	out, truncated, err := runGit(ctx, dir, maxGrepOutput, args...)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && len(out) == 0 {
		return nil, false, nil // no matches
	}
	if err != nil {
		return nil, false, err
	}
	prefix := commit + ":"
	perFile := map[string]int{}
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.SplitN(line, "\x00", 3)
		if len(f) != 3 {
			continue // empty, or cut off by the output limit
		}
		n, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		path := strings.TrimPrefix(f[0], prefix)
		if perFile[path]++; perFile[path] > maxGrepPerFile {
			truncated = true
			continue
		}
		if len(matches) == max {
			return matches, true, nil
		}
		matches = append(matches, GrepMatch{Path: path, Line: n, Text: f[2]})
	}
	return matches, truncated, nil
}

// DefaultBranch is the branch HEAD points to, or "" if it points to none.
func DefaultBranch(ctx context.Context, dir string) string {
	out, err := gitOutput(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Comparison is what changed from one commit to another.
type Comparison struct {
	Commits     []Commit // in to but not in from, newest first
	MoreCommits bool     // there are more than maxCompareCommits
	MergeBase   string   // "" if the two have no history in common
	Stat        string   // per file: lines added and removed (git diff --stat)
}

const maxCompareCommits = 250

// Compare lists the commits that to has and from doesn't, and what they
// changed per file (a diffstat, not the diff itself), from where they
// forked (their merge base) to to, like a pull request would. Without a
// common ancestor it is from from itself.
func Compare(ctx context.Context, dir, from, to string) (*Comparison, error) {
	if !HashRe.MatchString(from) || !HashRe.MatchString(to) {
		return nil, errNotFound
	}
	c := &Comparison{}
	commits, err := Log(ctx, dir, from+".."+to, "", 0, maxCompareCommits+1)
	if err != nil {
		return nil, err
	}
	if len(commits) > maxCompareCommits {
		commits, c.MoreCommits = commits[:maxCompareCommits], true
	}
	c.Commits = commits
	base := from
	if out, err := gitOutput(ctx, dir, "merge-base", from, to); err == nil {
		c.MergeBase = strings.TrimSpace(string(out))
		base = c.MergeBase
	}
	// See ReadCommit for the flags.
	diffArgs := []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", "-M"}
	stat, _, err := runGit(ctx, dir, 256<<10, append(diffArgs, "--stat=120,80", base, to, "--")...)
	if err != nil {
		return nil, err
	}
	c.Stat = strings.TrimRight(string(stat), "\n")
	return c, nil
}
