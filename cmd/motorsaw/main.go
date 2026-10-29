package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/wouteroostervld/motorsaw/pkg/config"
	"github.com/wouteroostervld/motorsaw/pkg/db"
	"github.com/wouteroostervld/motorsaw/pkg/embed"
	"github.com/wouteroostervld/motorsaw/pkg/filter"
	"github.com/wouteroostervld/motorsaw/pkg/indexer"
)

const version = "0.3.0"

const usage = `Usage: motorsaw <command>

Looking at code:
  grep <pattern> [path...]   Search with a regular expression; shows the definition around each match
  match <query> [path...]    Search the syntax tree with a tree-sitter query, in signatures or anywhere
  outline <file>             Show a file folded to its signatures
  query <cypher>             Query the code graph: definitions, calls, imports
  search <words>             Search by meaning (needs the embeddings index, which is off by default)

Keeping the index:
  index <path>               Index the source files under a path
  forget <path>              Make the index forget the files under a path; nothing on disk is touched
  status                     Show what is in the index, and which kinds of index are on here
  daemon start               Watch for changes and keep the index current
  migrate                    Move an installation made as chainsaw over to motorsaw

For Claude Code:
  hook read                  A PreToolUse hook that hands an agent the outline of a large file it reads whole

Run a command with --help for its options; "motorsaw version" says which motorsaw this is.`

func main() {
	if len(os.Args) < 2 {
		fmt.Println(usage)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "migrate", "version", "-v", "-V", "--version", "help", "-h", "--help", "hook":
	default:
		legacyNote()
	}

	switch os.Args[1] {
	case "grep":
		handleGrep(os.Args[2:])
	case "match":
		handleMatch(os.Args[2:])
	case "outline":
		handleOutline(os.Args[2:])
	case "query":
		handleQuery(os.Args[2:])
	case "search":
		handleSearch(os.Args[2:])
	case "graph":
		// "graph query" is the older spelling of "query"
		if len(os.Args) > 2 && os.Args[2] == "query" {
			handleQuery(os.Args[3:])
		} else {
			handleQuery([]string{"--help"})
		}
	case "index":
		handleIndex(os.Args[2:])
	case "forget":
		handleForget(os.Args[2:])
	case "status":
		handleStatus()
	case "daemon":
		handleDaemon()
	case "migrate":
		handleMigrate(os.Args[2:])
	case "hook":
		handleHook(os.Args[2:])
	case "version", "-v", "-V", "--version":
		fmt.Printf("motorsaw version %s\n", version)
	case "help", "-h", "--help":
		fmt.Println(usage)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n%s\n", os.Args[1], usage)
		os.Exit(1)
	}
}

// fail prints an error and exits
func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
	os.Exit(1)
}

// openDB opens the index
func openDB() *db.DB {
	database, err := db.Open(db.Config{Path: db.DefaultPath()})
	if err != nil {
		fail("opening the index: %v", err)
	}
	return database
}

// workingDir returns the absolute current directory, or an empty string
func workingDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return ""
	}
	return abs
}

// configPath is where the user's configuration is
func configPath() string {
	return filepath.Join(os.Getenv("HOME"), ".motorsaw", "config.yaml")
}

// loadProfile returns the active profile of the user's configuration, or
// defaults when there is no configuration. One that is there but cannot be
// used is an error: carrying on with defaults would quietly switch off what
// it switches on.
func loadProfile() (*config.Profile, error) {
	cfg, err := config.LoadGlobalConfigFromPath(configPath(), &config.RealFileSystem{})
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &config.Profile{Exclude: []string{"node_modules", "vendor", "__pycache__"}}, nil
	case err != nil:
		return nil, fmt.Errorf("%s: %w", configPath(), err)
	}
	return cfg.Profiles[cfg.ActiveProfile], nil
}

// activeProfile returns the active profile for a command that only needs to
// know what to skip, and can do without
func activeProfile() *config.Profile {
	profile, err := loadProfile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: %v; using defaults\n", err)
		return &config.Profile{Exclude: []string{"node_modules", "vendor", "__pycache__"}}
	}
	return profile
}

// settings is the configuration as the indexing commands use it
type settings struct {
	profile *config.Profile
	kinds   *config.IndexResolver // Which kinds of index are on, by file
	walk    *filter.Walk          // What a walk through a tree leaves out
}

// newWalk makes the rules for what a walk leaves out from a profile.
// Without gitignore, what git ignores is not left out, whatever the profile
// says.
func newWalk(profile *config.Profile, gitignore bool) *filter.Walk {
	if profile.Gitignore != nil && !*profile.Gitignore {
		gitignore = false
	}
	return filter.NewWalk(profile.Exclude, profile.Blacklist, profile.Whitelist, gitignore)
}

func loadSettings() (*settings, error) {
	profile, err := loadProfile()
	if err != nil {
		return nil, err
	}
	kinds, err := config.NewIndexResolver(profile, &config.RealFileSystem{})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", configPath(), err)
	}
	return &settings{profile: profile, kinds: kinds, walk: newWalk(profile, true)}, nil
}

// embedder returns the client for the configured embedding model, or nil
// when none is configured
func (s *settings) embedder() (*embed.Client, error) {
	e := s.profile.EmbeddingSettings()
	if e.Model == "" {
		return nil, nil
	}
	client, err := embed.New(embed.Config{Provider: e.Provider, BaseURL: e.BaseURL, APIKey: e.APIKey, Model: e.Model})
	if err != nil {
		return nil, fmt.Errorf("%s: embeddings: %w", configPath(), err)
	}
	return client, nil
}

// indexer returns the indexer with every kind of index there is, each
// switched on or off per file by the configuration. With kinds named, it
// works on those only.
func (s *settings) indexer(database *db.DB, only ...string) (*indexer.Indexer, error) {
	client, err := s.embedder()
	if err != nil {
		return nil, err
	}
	// A nil client must be a nil interface for the module to see it is missing
	var embedder indexer.Embedder
	if client != nil {
		embedder = client
	}

	graph := indexer.NewGraph(database)
	var include []string
	for _, dir := range s.profile.Include {
		if abs, err := config.NormalizePath(dir); err == nil {
			include = append(include, abs)
		}
	}
	graph.Projects = &indexer.Projects{
		Include: include,
		Follow: func(root string) ([]string, error) {
			return s.profile.FollowFor(root, &config.RealFileSystem{})
		},
	}

	idx := indexer.New(database, s.kinds.For, graph, indexer.NewEmbeddings(database, embedder))
	if len(only) > 0 {
		idx = idx.Only(only...)
	}
	return idx, nil
}

// embeddingsHelp says how to switch the embeddings index on
const embeddingsHelp = `The embeddings index is off by default: it asks a model for a vector for
every chunk of code, which is slow and may cost money. To switch it on, say
which model to use in ~/.motorsaw/config.yaml:

  profiles:
    default:
      index:
        embeddings: true          # everywhere; or leave this out and put
                                  # "index: {embeddings: true}" in the
                                  # .config.yaml of the projects that want it
      embeddings:
        model: nomic-embed-text   # served by Ollama on localhost, or with
        # base_url: https://api.openai.com/v1    any OpenAI-compatible server
        # api_key: ...`
