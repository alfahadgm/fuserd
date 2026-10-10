package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"fuseronline/internal/catalog"
	"fuseronline/internal/community"
	"fuseronline/internal/config"
	"fuseronline/internal/store"
)

const communityUsage = `usage:
  fuserd community add     -config config.json -x <X> -kind archive|folder -link <url> -file <id>,<size>,<name> ...
                           [-archive zip|rar4|rar5 -pak-entry <path> -sig-entry <path>]
                           [-artist <a>] [-title <t>] [-creator <c>] [-submitted <date>] [-sheet <gid>:<row>] [-note <text>]
  fuserd community add     -config config.json -json <entry.json | ->
  fuserd community update  -config config.json -x <X> [any add flag]   (only the flags given change)
  fuserd community import  -config config.json -batch <batch.jsonl | -> [-update]
  fuserd community remove  -config config.json -x <X>
  fuserd community disable -config config.json -x <X> [-reason <text>]
  fuserd community enable  -config config.json -x <X>
  fuserd community list    -config config.json [-creator <text>]
  fuserd community show    -config config.json -x <X>

Community songs are songs whose files stay on their creator's public Google Drive. An entry says
where <X>_P.pak and <X>_P.sig are: inside one archive (-file once, with -archive, -pak-entry and -sig-entry), or
loose in a folder (-file twice: the pak, then the sig). When a player's crate holds a song that the catalogue
doesn't, api/community/resolve/ answers with this entry and the loader downloads, extracts and checks the files.

import reads one JSON entry per line, the same object -json takes. It adds new entries and, with -update, replaces changed ones. A running fuserd sees
changes at once. Official shortnames and rows credited to Harmonix are refused.`

// fileList is a repeatable -file id,size,name flag.
type fileList []community.File

func (l *fileList) String() string { return fmt.Sprint(*l) }
func (l *fileList) Set(v string) error {
	parts := strings.SplitN(v, ",", 3)
	if len(parts) != 3 {
		return errors.New("-file takes <id>,<size>,<name>")
	}
	n, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return fmt.Errorf("-file size %q: %w", parts[1], err)
	}
	*l = append(*l, community.File{ID: parts[0], Size: n, Name: parts[2]})
	return nil
}

// communityCommand is "fuserd community ...": maintenance of the community song table.
func communityCommand(args []string, stdin io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(communityUsage)
	}
	cmd := args[0]
	fs := flag.NewFlagSet("community "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "config.json", "path to the configuration file")
	var song community.Song
	var files fileList
	var jsonPath, batch, reason, creator *string
	var update *bool
	switch cmd {
	case "add", "update":
		fs.StringVar(&song.Kind, "kind", "", "archive or folder")
		fs.StringVar(&song.Archive, "archive", "", "zip, rar4 or rar5")
		fs.Var(&files, "file", "a Drive file: <id>,<size>,<name> (repeat for a folder: the pak, then the sig)")
		fs.StringVar(&song.PakEntry, "pak-entry", "", "the pak's path inside the archive")
		fs.StringVar(&song.SigEntry, "sig-entry", "", "the sig's path inside the archive")
		fs.StringVar(&song.Link, "link", "", "the link as the sheet gives it")
		fs.StringVar(&song.Artist, "artist", "", "artist")
		fs.StringVar(&song.Title, "title", "", "title")
		fs.StringVar(&song.Creator, "creator", "", "the song's creator (as the sheet credits them)")
		fs.StringVar(&song.Submitted, "submitted", "", "the sheet's Submit Date")
		fs.StringVar(&song.Sheet, "sheet", "", "<gid>:<row> on the sheet")
		fs.StringVar(&song.Note, "note", "", "free text, such as the creator's permission")
		jsonPath = fs.String("json", "", "read the entry from a JSON file (- = stdin)")
	case "import":
		batch = fs.String("batch", "", "the batch: one JSON entry per line (- = stdin)")
		update = fs.Bool("update", false, "replace entries that changed")
	case "disable":
		reason = fs.String("reason", "disabled by an operator", "why the entry is disabled")
	case "list":
		creator = fs.String("creator", "", "only entries whose creator contains this")
	case "remove", "enable", "show":
	default:
		return errors.New(communityUsage)
	}
	if cmd != "import" && cmd != "list" {
		fs.StringVar(&song.X, "x", "", "the in-game shortname")
	}
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 {
		return errors.New(communityUsage)
	}
	if cmd != "import" && cmd != "list" && song.X == "" && (jsonPath == nil || *jsonPath == "") {
		return errors.New("-x is required\n" + communityUsage)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	check := entryChecker(cfg, out)

	switch cmd {
	case "add", "update":
		if *jsonPath != "" {
			if song, err = readEntry(*jsonPath, stdin); err != nil {
				return err
			}
			if cmd == "update" {
				if _, err := st.CommunitySong(ctx, song.X); err != nil {
					return fmt.Errorf("%s: %w", song.X, err)
				}
			}
		} else if cmd == "update" {
			cur, err := st.CommunitySong(ctx, song.X)
			if err != nil {
				return fmt.Errorf("%s: %w", song.X, err)
			}
			given := map[string]bool{}
			fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
			merged := cur.Song
			for name, dst := range map[string]*string{"kind": &merged.Kind, "archive": &merged.Archive,
				"pak-entry": &merged.PakEntry, "sig-entry": &merged.SigEntry, "link": &merged.Link,
				"artist": &merged.Artist, "title": &merged.Title, "creator": &merged.Creator,
				"submitted": &merged.Submitted, "sheet": &merged.Sheet, "note": &merged.Note} {
				if given[name] {
					*dst = *flagString(fs, name)
				}
			}
			if len(files) > 0 {
				merged.Files = files
			}
			song = merged
		} else {
			song.Files = files
		}
		if err := check(song); err != nil {
			return err
		}
		res, err := st.PutCommunitySong(ctx, song, cmd == "update")
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s %s (%s%s, %d file(s), creator %q)\n", res, song.X, song.Kind,
			map[bool]string{true: " " + song.Archive}[song.Archive != ""], len(song.Files), song.Creator)
		return nil
	case "import":
		if *batch == "" {
			return errors.New("-batch is required\n" + communityUsage)
		}
		return importEntries(ctx, st, *batch, stdin, *update, check, out)
	case "remove":
		if err := st.RemoveCommunitySong(ctx, song.X); err != nil {
			return fmt.Errorf("%s: %w", song.X, err)
		}
		fmt.Fprintf(out, "removed %s\n", song.X)
		return nil
	case "disable", "enable":
		why := ""
		if reason != nil {
			why = *reason
		}
		if err := st.SetCommunityDisabled(ctx, song.X, cmd == "disable", why); err != nil {
			return fmt.Errorf("%s: %w", song.X, err)
		}
		fmt.Fprintf(out, "%sd %s\n", cmd, song.X)
		return nil
	case "list":
		entries, err := st.ListCommunitySongs(ctx, *creator)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "X\tKIND\tARTIST\tTITLE\tCREATOR\tSHEET\tSTATE")
		for _, e := range entries {
			state := "enabled"
			if e.Disabled {
				state = "disabled: " + e.DisabledReason
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.X, strings.TrimSpace(e.Kind+" "+e.Archive), e.Artist, e.Title,
				e.Creator, e.Sheet, state)
		}
		fmt.Fprintf(w, "%d entr%s\n", len(entries), map[bool]string{true: "y", false: "ies"}[len(entries) == 1])
		return w.Flush()
	case "show":
		e, err := st.CommunitySong(ctx, song.X)
		if err != nil {
			return fmt.Errorf("%s: %w", song.X, err)
		}
		raw, _ := json.MarshalIndent(e.Song, "", "  ")
		fmt.Fprintf(out, "%s\nadded %s, updated %s, disabled %v %s\n", raw, e.AddedAt.UTC().Format("2006-01-02 15:04"),
			e.UpdatedAt.UTC().Format("2006-01-02 15:04"), e.Disabled, e.DisabledReason)
		reports, err := st.CommunityReports(ctx, e.X)
		if err != nil {
			return err
		}
		for _, r := range reports {
			fmt.Fprintf(out, "report %s: ok %v reason %q size %d sha256 %s\n", r.At.UTC().Format("2006-01-02 15:04"),
				r.OK, r.Reason, r.Size, r.SHA256)
		}
		return nil
	default:
		return errors.New(communityUsage)
	}
}

func flagString(fs *flag.FlagSet, name string) *string {
	v := fs.Lookup(name).Value.String()
	return &v
}

// entryChecker refuses official shortnames, and notes a shortname the catalogue already has (the catalogue
// answers first, so the community entry is only a fallback for it).
func entryChecker(cfg config.Config, out io.Writer) func(community.Song) error {
	checker := community.NewChecker(cfg.SongManifest.Songs, cfg.Store.Reserved)
	var cat *catalog.Catalog
	if cfg.Store.CatalogueFile != "" {
		cat, _ = catalog.Load(cfg.Store.CatalogueFile, cfg.Store.FilesDir)
	}
	return func(s community.Song) error {
		if err := checker.Check(&s); err != nil {
			return err
		}
		if cat != nil {
			if _, ok := cat.ByShortName(s.X); ok {
				fmt.Fprintf(out, "note: %s is also in the catalogue, which answers first\n", s.X)
			}
		}
		return nil
	}
}

func openInput(path string, stdin io.Reader) (io.Reader, func(), error) {
	if path == "-" {
		return stdin, func() {}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { f.Close() }, nil
}

func readEntry(path string, stdin io.Reader) (community.Song, error) {
	var s community.Song
	r, done, err := openInput(path, stdin)
	if err != nil {
		return s, err
	}
	defer done()
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// importEntries applies a batch: one JSON entry per line; blank lines and lines starting with # are skipped. A bad
// line is reported and skipped, so one row can't stop a batch.
func importEntries(ctx context.Context, st *store.Store, path string, stdin io.Reader, update bool,
	check func(community.Song) error, out io.Writer) error {
	r, done, err := openInput(path, stdin)
	if err != nil {
		return err
	}
	defer done()
	counts := map[string]int{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var s community.Song
		dec := json.NewDecoder(strings.NewReader(text))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			fmt.Fprintf(out, "line %d: refused: %v\n", line, err)
			counts["refused"]++
			continue
		}
		if err := check(s); err != nil {
			fmt.Fprintf(out, "line %d: refused: %v\n", line, err)
			counts["refused"]++
			continue
		}
		res, err := st.PutCommunitySong(ctx, s, update)
		if errors.Is(err, store.ErrCommunityExists) {
			fmt.Fprintf(out, "line %d: %s exists (kept; -update replaces it)\n", line, s.X)
			counts["kept"]++
			continue
		}
		if err != nil {
			fmt.Fprintf(out, "line %d: refused: %v\n", line, err)
			counts["refused"]++
			continue
		}
		if res != store.CommunityUnchanged {
			fmt.Fprintf(out, "line %d: %s %s (%s)\n", line, res, s.X, s.Creator)
		}
		counts[res]++
	}
	if err := sc.Err(); err != nil {
		return err
	}
	fmt.Fprintf(out, "import: %d added, %d updated, %d unchanged, %d kept, %d refused\n", counts[store.CommunityAdded],
		counts[store.CommunityUpdated], counts[store.CommunityUnchanged], counts["kept"], counts["refused"])
	return nil
}
