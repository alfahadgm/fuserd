package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"fuseronline/internal/catalog"
	"fuseronline/internal/config"
)

const catalogueUsage = `usage:
  fuserd catalogue add  -config config.json -pak <song>_P.pak [-sig <song>_P.sig] -licence <text>
                        [-source <url>] [-name <display name>] [-song <title>] [-artist <name>] [-album <name>]
                        [-genre <EGenre name or index>] [-year <n>] [-art <cover.png|.jpg>] [-update]
  fuserd catalogue publish   -config config.json -shortname <sn> -price <elder credits> [-featured]
                             [-start <ISO 8601>] [-end <ISO 8601>]
  fuserd catalogue unpublish -config config.json -shortname <sn>
  fuserd catalogue list      -config config.json

add inspects a community song pak (mount point, a single song Meta, nothing outside the song's own folders), checks
its .sig, copies both into store.files_dir and records the entry in store.catalogue_file. A running fuserd picks the
change up within a few seconds. -update replaces an existing entry's files and bumps its version. -art adds album
art, shown on store tiles before the song is installed.

publish offers a catalogue song in the game's Elder Credit ("Diamond") store; without -end the offer never ends.
unpublish withdraws it (players who bought it keep it).`

// catalogueCommand is "fuserd catalogue ...": maintenance of the community song catalogue.
func catalogueCommand(args []string) error {
	if len(args) == 0 {
		return errors.New(catalogueUsage)
	}
	fs := flag.NewFlagSet("catalogue "+args[0], flag.ContinueOnError)
	configPath := fs.String("config", "config.json", "path to the configuration file")
	pak := fs.String("pak", "", "the song's _P.pak")
	sig := fs.String("sig", "", "its .sig (default: the pak path with .sig)")
	licence := fs.String("licence", "", "the licence or permission the song is shared under (required)")
	source := fs.String("source", "", "where the song came from (URL, author)")
	name := fs.String("name", "", "display name (default: the shortname)")
	song := fs.String("song", "", "song title (default: the display name)")
	artist := fs.String("artist", "", "artist (default: Community)")
	album := fs.String("album", "", "album")
	genre := fs.String("genre", "0", "EGenre name or index ("+strings.Join(catalog.Genres, ", ")+")")
	year := fs.Int("year", 0, "release year")
	update := fs.Bool("update", false, "replace an existing entry's files (bumps its version)")
	art := fs.String("art", "", "album art image (.png or .jpg)")
	shortName := fs.String("shortname", "", "the catalogue song to publish / unpublish")
	price := fs.Int("price", -1, "the Elder Credit price")
	featured := fs.Bool("featured", false, "show it as a featured offer (the game shows 2)")
	start := fs.String("start", "", "when the offer opens (ISO 8601; default: now)")
	end := fs.String("end", "", "when the offer ends (ISO 8601; default: never)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.Store.CatalogueFile == "" {
		return errors.New("store.catalogue_file is not set in the configuration")
	}
	if args[0] != "list" {
		unlock, err := catalog.Lock(cfg.Store.CatalogueFile)
		if err != nil {
			return err
		}
		defer unlock()
	}
	c, err := catalog.Load(cfg.Store.CatalogueFile, cfg.Store.FilesDir)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "SHORTNAME\tVERSION\tNAME\tARTIST\tPAK BYTES\tLICENCE\tELDER CREDIT STORE")
		for _, it := range c.Items() {
			offer := "-"
			if l := it.ElderCredit; l != nil {
				offer = fmt.Sprintf("%d credits%s, until %s", l.Price, map[bool]string{true: ", featured"}[l.Featured], l.EndDate)
			}
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%d\t%s\t%s\n", it.ShortName, it.Version, it.Name, it.Artist.Name, it.PakSize,
				it.Licence, offer)
		}
		return w.Flush()
	case "publish", "unpublish":
		if *shortName == "" {
			return errors.New("-shortname is required\n" + catalogueUsage)
		}
		var it catalog.Item
		if args[0] == "unpublish" {
			it, err = c.Unpublish(*shortName)
		} else {
			if *price < 0 {
				return errors.New("-price is required\n" + catalogueUsage)
			}
			if *start == "" {
				*start = time.Now().UTC().Format(time.RFC3339)
			}
			it, err = c.Publish(*shortName, catalog.Listing{Price: int32(*price), Featured: *featured,
				StartDate: *start, EndDate: *end})
		}
		if err != nil {
			return err
		}
		if err := c.Save(); err != nil {
			return err
		}
		if it.ElderCredit == nil {
			fmt.Printf("%s is no longer offered in the Elder Credit store\n", it.ShortName)
		} else {
			fmt.Printf("%s offered for %d Elder Credits (featured %v), %s to %s\n", it.ShortName, it.ElderCredit.Price,
				it.ElderCredit.Featured, it.ElderCredit.StartDate, it.ElderCredit.EndDate)
		}
		return nil
	case "add":
		if *pak == "" {
			return errors.New("-pak is required\n" + catalogueUsage)
		}
		if *sig == "" {
			*sig = strings.TrimSuffix(*pak, ".pak") + ".sig"
		}
		g, err := genreID(*genre)
		if err != nil {
			return err
		}
		reserved := map[string]bool{}
		for _, sn := range append(append([]string{}, cfg.SongManifest.Songs...), cfg.Store.Reserved...) {
			reserved[strings.ToLower(sn)] = true
		}
		it, err := c.Add(catalog.AddOptions{PakPath: *pak, SigPath: *sig, Licence: *licence, Source: *source,
			Name: *name, SongName: *song, Artist: *artist, Album: *album, GenreID: g, Year: int32(*year),
			Reserved: reserved, MaxPakBytes: int64(cfg.Store.MaxPakMB) << 20, Update: *update, ArtPath: *art})
		if errors.Is(err, catalog.ErrUnchanged) {
			fmt.Printf("%s version %d is already in the catalogue with these exact files\n", it.ShortName, it.Version)
			return nil
		}
		if err != nil {
			return err
		}
		if err := c.Save(); err != nil {
			return err
		}
		fmt.Printf("added %s version %d\n  meta_path  %s\n  pak        %d bytes, sha256 %s\n  sig        %d bytes, sha256 %s\n  art        %s\n  files in   %s\n",
			it.ShortName, it.Version, it.MetaPath, it.PakSize, it.PakSHA256, it.SigSize, it.SigSHA256, or(it.Art, "-"), c.Dir(it))
		return nil
	default:
		return errors.New(catalogueUsage)
	}
}

func genreID(s string) (int32, error) {
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 || n >= len(catalog.Genres) {
			return 0, fmt.Errorf("genre %d is not an EGenre index", n)
		}
		return int32(n), nil
	}
	for i, g := range catalog.Genres {
		if strings.EqualFold(g, s) {
			return int32(i), nil
		}
	}
	return 0, fmt.Errorf("unknown genre %q (%s)", s, strings.Join(catalog.Genres, ", "))
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
