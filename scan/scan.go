package scan

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/zricethezav/gitleaks/v6/manager"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	fdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/sergi/go-diff/diffmatchpatch"
	log "github.com/sirupsen/logrus"
)

// Bundle contains various git information for scans.
type Bundle struct {
	Commit    *object.Commit
	Patch     string
	Content   string
	FilePath  string
	Operation fdiff.Operation

	reader     io.Reader
	lineLookup map[string]bool
	scanType   int
}

// commitScanner is a function signature for scanning commits. There is some
// redundant work needed by scanning all files at a commit (--files-at-commit=) and scanning
// the patches generated by a commit (--commit=). The function scanCommit wraps that redundant work
// and accepts a commitScanner for the different logic needed between the two cases described above.
type commitScanner func(c *object.Commit, repo *Repo) error

const (
	// We need to differentiate between scans as the logic for line searching is different between
	// scanning patches, commits, and uncommitted files.
	patchScan int = iota + 1
	uncommittedScan
	commitScan
)

// Scan is responsible for scanning the entire history (default behavior) of a
// git repo. Options that can change the behavior of this function include: --Commit, --depth, --branch.
// See options/options.go for an explanation on these options.
func (repo *Repo) Scan() error {
	if err := repo.setupTimeout(); err != nil {
		return err
	}
	if repo.cancel != nil {
		defer repo.cancel()
	}

	if repo.Repository == nil {
		return fmt.Errorf("%s repo is empty", repo.Name)
	}

	// load up alternative config if possible, if not use manager's config
	if repo.Manager.Opts.RepoConfig {
		cfg, err := repo.loadRepoConfig()
		if err != nil {
			return err
		}
		repo.config = cfg
	}

	scanTimeStart := time.Now()

	// See https://github.com/zricethezav/gitleaks/issues/326
	// Scan commit patches, all files at a commit, or a range of commits
	if repo.Manager.Opts.Commit != "" {
		return scanCommit(repo.Manager.Opts.Commit, repo, scanCommitPatches)
	} else if repo.Manager.Opts.FilesAtCommit != "" {
		return scanCommit(repo.Manager.Opts.FilesAtCommit, repo, scanFilesAtCommit)
	} else if repo.Manager.Opts.Commits != "" {
		commits := strings.Split(repo.Manager.Opts.Commits, ",")
		for _, c := range commits {
			err := scanCommit(c, repo, scanCommitPatches)
			if err != nil {
				return err
			}
		}
		return nil
	} else if repo.Manager.Opts.CommitsFile != "" {
		file, err := os.Open(repo.Manager.Opts.CommitsFile)
		if err != nil {
			return err
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			err := scanCommit(scanner.Text(), repo, scanCommitPatches)
			if err != nil {
				return err
			}
		}
		return nil
	}

	logOpts, err := getLogOptions(repo)
	if err != nil {
		return err
	}
	cIter, err := repo.Log(logOpts)
	if err != nil {
		return err
	}

	cc := 0
	semaphore := make(chan bool, howManyThreads(repo.Manager.Opts.Threads))
	wg := sync.WaitGroup{}
	err = cIter.ForEach(func(c *object.Commit) error {
		if c == nil || repo.timeoutReached() || repo.depthReached(cc) {
			return storer.ErrStop
		}

		// Check if Commit is allowlisted
		if isCommitAllowListed(c.Hash.String(), repo.config.Allowlist.Commits) {
			return nil
		}

		// Check if at root
		if len(c.ParentHashes) == 0 {
			cc++
			err = scanFilesAtCommit(c, repo)
			if err != nil {
				return err
			}
			return nil
		}

		// increase Commit counter
		cc++

		// inspect first parent only as all other parents will be eventually reached
		// (they exist as the tip of other branches, etc)
		// See https://github.com/zricethezav/gitleaks/issues/413 for details
		parent, err := c.Parent(0)
		if err != nil {
			return err
		}

		defer func() {
			if err := recover(); err != nil {
				// sometimes the Patch generation will fail due to a known bug in
				// sergi's go-diff: https://github.com/sergi/go-diff/issues/89.
				// Once a fix has been merged I will remove this recover.
				return
			}
		}()
		if repo.timeoutReached() {
			return nil
		}
		if parent == nil {
			// shouldn't reach this point but just in case
			return nil
		}

		start := time.Now()
		patch, err := parent.Patch(c)
		if err != nil {
			log.Errorf("could not generate Patch")
		}
		repo.Manager.RecordTime(manager.PatchTime(howLong(start)))

		wg.Add(1)
		semaphore <- true
		go func(c *object.Commit, patch *object.Patch) {
			defer func() {
				<-semaphore
				wg.Done()
			}()
			scanPatch(patch, c, repo)
		}(c, patch)

		if c.Hash.String() == repo.Manager.Opts.CommitTo {
			return storer.ErrStop
		}
		return nil
	})

	wg.Wait()
	repo.Manager.RecordTime(manager.ScanTime(howLong(scanTimeStart)))
	repo.Manager.IncrementCommits(cc)
	return nil
}

// scanEmpty scans an empty repo without any commits. See https://github.com/zricethezav/gitleaks/issues/352
func (repo *Repo) scanEmpty() error {
	scanTimeStart := time.Now()
	wt, err := repo.Worktree()
	if err != nil {
		return err
	}

	status, err := wt.Status()
	if err != nil {
		return err
	}
	for fn := range status {
		workTreeBuf := bytes.NewBuffer(nil)
		workTreeFile, err := wt.Filesystem.Open(fn)
		if err != nil {
			continue
		}
		if _, err := io.Copy(workTreeBuf, workTreeFile); err != nil {
			return err
		}
		repo.CheckRules(&Bundle{
			Content:  workTreeBuf.String(),
			FilePath: workTreeFile.Name(),
			Commit:   emptyCommit(),
			scanType: uncommittedScan,
		})
	}
	repo.Manager.RecordTime(manager.ScanTime(howLong(scanTimeStart)))
	return nil
}

// scanUncommitted will do a `git diff` and scan changed files that are being tracked. This is useful functionality
// for a pre-Commit hook so you can make sure your code does not have any leaks before committing.
func (repo *Repo) scanUncommitted() error {
	// load up alternative config if possible, if not use manager's config
	if repo.Manager.Opts.RepoConfig {
		cfg, err := repo.loadRepoConfig()
		if err != nil {
			return err
		}
		repo.config = cfg
	}

	if err := repo.setupTimeout(); err != nil {
		return err
	}

	r, err := repo.Head()
	if err == plumbing.ErrReferenceNotFound {
		// possibly an empty repo, or maybe its not, either way lets scan all the files in the directory
		return repo.scanEmpty()
	} else if err != nil {
		return err
	}

	scanTimeStart := time.Now()

	c, err := repo.CommitObject(r.Hash())
	if err != nil {
		return err
	}
	// Staged change so the Commit details do not yet exist. Insert empty defaults.
	c.Hash = plumbing.Hash{}
	c.Message = "***STAGED CHANGES***"
	c.Author.Name = ""
	c.Author.Email = ""
	c.Author.When = time.Unix(0, 0).UTC()

	prevTree, err := c.Tree()
	if err != nil {
		return err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return err
	}

	status, err := getStagedChanges(wt)
	for _,fn := range status {
		var (
			prevFileContents string
			currFileContents string
			filename         string
		)
		
		workTreeBuf := bytes.NewBuffer(nil)
		workTreeFile, err := wt.Filesystem.Open(fn)
		if err != nil {
			continue
		}
		if _, err := io.Copy(workTreeBuf, workTreeFile); err != nil {
			return err
		}
		currFileContents = workTreeBuf.String()
		filename = workTreeFile.Name()

		// get files at HEAD state
		prevFile, err := prevTree.File(fn)
		if err != nil {
			prevFileContents = ""

		} else {
			prevFileContents, err = prevFile.Contents()
			if err != nil {
				return err
			}
			if filename == "" {
				filename = prevFile.Name
			}
		}

		dmp := diffmatchpatch.New()
		diffs := dmp.DiffCleanupSemantic(dmp.DiffMain(prevFileContents, currFileContents, false))
		var diffContents string
		for _, d := range diffs {
			if d.Type == diffmatchpatch.DiffInsert {
				diffContents += fmt.Sprintf("%s\n", d.Text)
			}
		}
		repo.CheckRules(&Bundle{
			Content:  diffContents,
			FilePath: filename,
			Commit:   c,
			scanType: uncommittedScan,
		})
	}

	if err != nil {
		return err
	}
	repo.Manager.RecordTime(manager.ScanTime(howLong(scanTimeStart)))
	return nil
}

// gitStatus returns the status of modified files in the worktree. It will attempt to execute 'git status'
// and will fall back to git.Worktree.Status() if that fails.
func gitStatus(wt *git.Worktree) (git.Status, error) {
	c := exec.Command("git", "status", "--porcelain", "-z")
	c.Dir = wt.Filesystem.Root()
	output, err := c.Output()
	if err != nil {
		stat, err := wt.Status()
		return stat, err
	}

	lines := strings.Split(string(output), "\000")
	stat := make(map[string]*git.FileStatus, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}

		// For copy/rename the output looks like
		//   R  destination\000source
		// Which means we can split on space and ignore anything with only one result
		parts := strings.SplitN(strings.TrimLeft(line, " "), " ", 2)
		if len(parts) == 2 {
			stat[strings.Trim(parts[1], " ")] = &git.FileStatus{
				Staging: git.StatusCode([]byte(parts[0])[0]),
			}
		}
	}
	return stat, err
}

//run the command "git diff --cached --name-status --diff-filter=ACM" to get all the staged files that have 
//been modified, added or copied.
func getStagedChanges(wt *git.Worktree) ([]string, error){
	var stagedFiles []string

	c := exec.Command("git", "diff", "--cached", "--name-status", "--diff-filter=ACM")
	c.Dir = wt.Filesystem.Root()
	output, err := c.CombinedOutput()

	if err != nil {
		log.Fatal("Execution of git command failed\n")
	}
	
	//list staged files in format "Status \t Filename" e.g "A	new-file"
	stagedFilesAndStatus := strings.Split(string(output), "\n")
	fmt.Printf("files %v\n", stagedFilesAndStatus)

	for _, fileString := range stagedFilesAndStatus {
		if len(fileString) != 0 {

			//extract file name only and add to array
			fileStatusAndName := strings.Split(fileString, "\t")
			if len(fileStatusAndName) > 0 {
				file := fileStatusAndName[1]
				stagedFiles = append(stagedFiles, file)
			}
		}
	}
	return stagedFiles, err
}

// scan accepts a Patch, Commit, and repo. If the patches contains files that are
// binary, then gitleaks will skip scanning that file OR if a file is matched on
// allowlisted files set in the configuration. If a global rule for files is defined and a filename
// matches said global rule, then a leak is sent to the manager.
// After that, file chunks are created which are then inspected by InspectString()
func scanPatch(patch *object.Patch, c *object.Commit, repo *Repo) {
	bundle := Bundle{
		Commit:   c,
		Patch:    patch.String(),
		scanType: patchScan,
	}
	for _, f := range patch.FilePatches() {
		if repo.timeoutReached() {
			return
		}
		if f.IsBinary() {
			continue
		}
		for _, chunk := range f.Chunks() {
			if chunk.Type() == fdiff.Add || (repo.Manager.Opts.Deletion && chunk.Type() == fdiff.Delete) {
				bundle.Content = chunk.Content()
				bundle.Operation = chunk.Type()

				// get filepath
				from, to := f.Files()
				if from != nil {
					bundle.FilePath = from.Path()
				} else if to != nil {
					bundle.FilePath = to.Path()
				} else {
					bundle.FilePath = "???"
				}
				repo.CheckRules(&bundle)
			}
		}
	}
}

// scanCommit accepts a Commit hash, repo, and commit scanning function. A new Commit
// object will be created from the hash which will be passed into either scanCommitPatches
// or scanFilesAtCommit depending on the options set.
func scanCommit(commit string, repo *Repo, f commitScanner) error {
	if commit == "latest" {
		ref, err := repo.Repository.Head()
		if err != nil {
			return err
		}
		commit = ref.Hash().String()
	}
	repo.Manager.IncrementCommits(1)
	h := plumbing.NewHash(commit)
	c, err := repo.CommitObject(h)
	if err != nil {
		return err
	}
	return f(c, repo)
}

// scanCommitPatches accepts a Commit object and a repo. This function is only called when the --Commit=
// option has been set. That option tells gitleaks to look only at a single Commit and check the contents
// of said Commit. Similar to scan(), if the files contained in the Commit are a binaries or if they are
// allowlisted then those files will be skipped.
func scanCommitPatches(c *object.Commit, repo *Repo) error {
	if len(c.ParentHashes) == 0 {
		err := scanFilesAtCommit(c, repo)
		if err != nil {
			return err
		}
	}

	return c.Parents().ForEach(func(parent *object.Commit) error {
		defer func() {
			if err := recover(); err != nil {
				// sometimes the Patch generation will fail due to a known bug in
				// sergi's go-diff: https://github.com/sergi/go-diff/issues/89.
				// Once a fix has been merged I will remove this recover.
				return
			}
		}()
		if repo.timeoutReached() {
			return nil
		}
		if parent == nil {
			return nil
		}
		start := time.Now()
		patch, err := parent.Patch(c)
		if err != nil {
			return fmt.Errorf("could not generate Patch")
		}
		repo.Manager.RecordTime(manager.PatchTime(howLong(start)))

		scanPatch(patch, c, repo)

		return nil
	})
}

// scanFilesAtCommit accepts a Commit object and a repo. This function is only called when the --files-at-Commit=
// option has been set. That option tells gitleaks to look only at ALL the files at a Commit and check the contents
// of said Commit. Similar to scan(), if the files contained in the Commit are a binaries or if they are
// allowlisted then those files will be skipped.
func scanFilesAtCommit(c *object.Commit, repo *Repo) error {
	fIter, err := c.Files()
	if err != nil {
		return err
	}

	err = fIter.ForEach(func(f *object.File) error {
		bin, err := f.IsBinary()
		if bin || repo.timeoutReached() {
			return nil
		} else if err != nil {
			return err
		}

		content, err := f.Contents()
		if err != nil {
			return err
		}

		repo.CheckRules(&Bundle{
			Content:   content,
			FilePath:  f.Name,
			Commit:    c,
			scanType:  commitScan,
			Operation: fdiff.Add,
		})
		return nil
	})
	return err
}

// depthReached checks if i meets the depth (--depth=) if set
func (repo *Repo) depthReached(i int) bool {
	if repo.Manager.Opts.Depth != 0 && repo.Manager.Opts.Depth == i {
		log.Warnf("Exceeded depth limit (%d)", i)
		return true
	}
	return false
}

// emptyCommit generates an empty commit used for scanning uncommitted changes
func emptyCommit() *object.Commit {
	return &object.Commit{
		Hash:    plumbing.Hash{},
		Message: "***STAGED CHANGES***",
		Author: object.Signature{
			Name:  "",
			Email: "",
			When:  time.Unix(0, 0).UTC(),
		},
	}
}
