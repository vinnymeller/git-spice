package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v61/github"
	"github.com/rogpeppe/go-internal/diff"
	"github.com/rogpeppe/go-internal/testscript"
	"go.abhg.dev/gs/internal/gh/ghtest"
	"go.abhg.dev/gs/internal/git/gittest"
	"go.abhg.dev/gs/internal/logtest"
	"go.abhg.dev/gs/internal/mockedit"
	"go.abhg.dev/gs/internal/termtest"
)

var _update = flag.Bool("update", false, "update golden files")

func TestMain(m *testing.M) {
	testscript.RunMain(m, map[string]func() int{
		"gs": func() int {
			main()
			return 0
		},
		"mockedit": mockedit.Main,
		// "true" is a no-op command that always succeeds.
		"true": func() int { return 0 },
		// with-term file -- cmd [args ...]
		//
		// Runs the given command inside a terminal emulator,
		// using the file to drive interactions with it.
		// See [termtest.WithTerm] for supported commands.
		"with-term": termtest.WithTerm,
	})
}

func TestScript(t *testing.T) {
	// We always put a *shamHubValue into the environment
	// because testscript does not allow adding the value afterwards.
	// We only set up the ShamHub on gh-init, though.
	type shamHubKey struct{}
	type shamHubValue struct{ v *ghtest.ShamHub }

	type testingTBKey struct{}

	defaultEnv := gittest.DefaultConfig().EnvMap()
	defaultEnv["EDITOR"] = "mockedit"

	// Add a default author to all commits.
	// Tests can override with 'as' and 'at'.
	defaultEnv["GIT_AUTHOR_NAME"] = "Test"
	defaultEnv["GIT_AUTHOR_EMAIL"] = "test@example.com"
	defaultEnv["GIT_COMMITTER_NAME"] = "Test"
	defaultEnv["GIT_COMMITTER_EMAIL"] = "test@example.com"

	testscript.Run(t, testscript.Params{
		Dir:                filepath.Join("testdata", "script"),
		UpdateScripts:      *_update,
		RequireUniqueNames: true,
		Setup: func(e *testscript.Env) error {
			for k, v := range defaultEnv {
				e.Setenv(k, v)
			}

			e.Values[shamHubKey{}] = &shamHubValue{}
			e.Values[testingTBKey{}] = e.T().(testing.TB)
			return nil
		},
		Cmds: map[string]func(*testscript.TestScript, bool, []string){
			"git":        gittest.CmdGit,
			"as":         gittest.CmdAs,
			"at":         gittest.CmdAt,
			"cmpenvJSON": cmdCmpenvJSON,

			// Sets up a fake GitHub server.
			"gh-init": func(ts *testscript.TestScript, neg bool, args []string) {
				t := ts.Value(testingTBKey{}).(testing.TB)
				shamHub, err := ghtest.NewShamHub(ghtest.ShamHubConfig{
					Log: logtest.New(t),
				})
				if err != nil {
					ts.Fatalf("create ShamHub: %s", err)
				}
				ts.Defer(func() {
					if err := shamHub.Close(); err != nil {
						ts.Logf("close ShamHub: %s", err)
					}
				})
				ts.Value(shamHubKey{}).(*shamHubValue).v = shamHub

				ts.Logf("Set up ShamHub:\n"+
					"  API URL  = %s\n"+
					"  Git URL  = %s\n"+
					"  Git root = %s",
					shamHub.APIURL(),
					shamHub.GitURL(),
					shamHub.GitRoot(),
				)

				ts.Setenv("GITHUB_API_URL", shamHub.APIURL())
				ts.Setenv("GITHUB_GIT_URL", shamHub.GitURL())
				ts.Setenv("GITHUB_TOKEN", "test-token")
			},

			// Clones a repository from the fake GitHub server.
			"gh-clone": func(ts *testscript.TestScript, neg bool, args []string) {
				if neg || len(args) != 2 {
					ts.Fatalf("usage: gh-clone <owner/repo> <dir>")
				}

				shamHub := ts.Value(shamHubKey{}).(*shamHubValue).v
				if shamHub == nil {
					ts.Fatalf("gh-add-remote: ShamHub not initialized")
				}

				ownerRepo, dir := args[0], args[1]
				owner, repo, ok := strings.Cut(ownerRepo, "/")
				if !ok {
					ts.Fatalf("invalid owner/repo: %s", ownerRepo)
				}

				err := ts.Exec("git", "clone", shamHub.RepoURL(owner, repo), dir)
				if err != nil {
					ts.Fatalf("git clone: %s", err)
				}
			},

			// gh-merge <owner/repo> <pr>
			"gh-merge": func(ts *testscript.TestScript, neg bool, args []string) {
				if neg || len(args) != 2 {
					ts.Fatalf("usage: gh-merge <owner/repo> <pr>")
				}

				shamHub := ts.Value(shamHubKey{}).(*shamHubValue).v
				if shamHub == nil {
					ts.Fatalf("gh-merge: ShamHub not initialized")
				}

				ownerRepo, prStr := args[0], args[1]
				owner, repo, ok := strings.Cut(ownerRepo, "/")
				if !ok {
					ts.Fatalf("invalid owner/repo: %s", ownerRepo)
				}
				pr, err := strconv.Atoi(prStr)
				if err != nil {
					ts.Fatalf("invalid PR number: %s", err)
				}

				req := ghtest.MergePullRequest{
					Owner:  owner,
					Repo:   repo,
					Number: pr,
				}
				if at := ts.Getenv("GIT_COMMITTER_DATE"); at != "" {
					t, err := time.Parse(time.RFC3339, at)
					if err != nil {
						ts.Fatalf("invalid time: %s", err)
					}
					req.Time = t
				}

				if name := ts.Getenv("GIT_COMMITTER_NAME"); name != "" {
					req.CommitterName = name
				}
				if email := ts.Getenv("GIT_COMMITTER_EMAIL"); email != "" {
					req.CommitterEmail = email
				}

				ts.Check(shamHub.MergePull(req))
			},

			"gh-add-remote": func(ts *testscript.TestScript, neg bool, args []string) {
				if neg || len(args) != 2 {
					ts.Fatalf("usage: gh-add-remote <remote> <owner/repo>")
				}

				shamHub := ts.Value(shamHubKey{}).(*shamHubValue).v
				if shamHub == nil {
					ts.Fatalf("gh-add-remote: ShamHub not initialized")
				}

				remote, ownerRepo := args[0], args[1]
				owner, repo, ok := strings.Cut(ownerRepo, "/")
				if !ok {
					ts.Fatalf("invalid owner/repo: %s", ownerRepo)
				}
				repo = strings.TrimSuffix(repo, ".git")
				repoURL, err := shamHub.NewRepository(owner, repo)
				if err != nil {
					ts.Fatalf("create repository: %s", err)
				}

				ts.Check(ts.Exec("git", "remote", "add", remote, repoURL))
			},

			"gh-dump-pull": func(ts *testscript.TestScript, neg bool, args []string) {
				if neg || len(args) > 1 {
					ts.Fatalf("usage: gh-dump-pull [n]")
				}

				shamHub := ts.Value(shamHubKey{}).(*shamHubValue).v
				if shamHub == nil {
					ts.Fatalf("gh-dump-pulls: ShamHub not initialized")
				}

				pulls, err := shamHub.ListPulls()
				if err != nil {
					ts.Fatalf("list pulls: %s", err)
				}

				var give any
				if len(args) == 0 {
					give = pulls
				} else {
					wantPR, err := strconv.Atoi(args[0])
					if err != nil {
						ts.Fatalf("invalid PR number: %s", err)
					}

					idx := slices.IndexFunc(pulls, func(pr *github.PullRequest) bool {
						return pr.GetNumber() == wantPR
					})
					if idx < 0 {
						ts.Fatalf("PR %d not found", wantPR)
					}
					give = pulls[idx]
				}

				enc := json.NewEncoder(ts.Stdout())
				enc.SetIndent("", "  ")
				ts.Check(enc.Encode(give))
			},
		},
	})
}

func cmdCmpenvJSON(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 2 {
		ts.Fatalf("usage: cmpjson file1 file2")
	}
	name1, name2 := args[0], args[1]

	data1 := []byte(ts.ReadFile(name1))
	data2, err := os.ReadFile(ts.MkAbs(name2))
	ts.Check(err)

	// Expand environment variables in data2.
	data2 = []byte(os.Expand(string(data2), ts.Getenv))

	var json1, json2 any
	ts.Check(json.Unmarshal(data1, &json1))
	ts.Check(json.Unmarshal(data2, &json2))

	if reflect.DeepEqual(json1, json2) == !neg {
		// Matches expectation.
		return
	}

	prettyJSON1, err := json.MarshalIndent(json1, "", "  ")
	ts.Check(err)

	if neg {
		ts.Logf("%s", prettyJSON1)
		ts.Fatalf("%s and %s do not differ", name1, name2)
		return
	}

	prettyJSON2, err := json.MarshalIndent(json2, "", "  ")
	ts.Check(err)

	unifiedDiff := diff.Diff(name1, prettyJSON1, name2, prettyJSON2)
	ts.Logf("%s", unifiedDiff)
	ts.Fatalf("%s and %s differ", name1, name2)
}
