package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

const name = "nostr-runbot"

const version = "0.0.2"

var revision = "HEAD"

// langToCompiler maps the `lang` part of `/run <lang>` to a wandbox compiler name.
// wandbox has dropped most *-head entries, so pinned stable versions are used.
// The up-to-date list is available at https://wandbox.org/api/list.json.
var langToCompiler = map[string]string{
	"rb":     "ruby-3.4.1",
	"ruby":   "ruby-3.4.1",
	"py":     "cpython-3.13.8",
	"python": "cpython-3.13.8",
	"go":     "go-1.23.2",
	"js":     "nodejs-20.17.0",
	"node":   "nodejs-20.17.0",
	"c":      "gcc-head-c",
	"cpp":    "gcc-head",
	"c++":    "gcc-head",
	"rs":     "rust-1.82.0",
	"rust":   "rust-1.82.0",
	"sh":     "bash",
	"bash":   "bash",
	"php":    "php-8.3.12",
	"pl":     "perl-5.42.0",
	"perl":   "perl-5.42.0",
	"lua":    "lua-5.4.7",
	"swift":  "swift-6.0.1",
}

type wandboxRequest struct {
	Code     string `json:"code"`
	Compiler string `json:"compiler"`
	Save     bool   `json:"save"`
}

type wandboxResponse struct {
	Status         string `json:"status"`
	CompilerError  string `json:"compiler_error"`
	CompilerOutput string `json:"compiler_output"`
	ProgramError   string `json:"program_error"`
	ProgramOutput  string `json:"program_output"`
	Signal         string `json:"signal"`
}

// listLanguages returns a human-readable list of supported languages, grouping aliases per compiler.
func listLanguages() string {
	groups := map[string][]string{}
	for alias, compiler := range langToCompiler {
		groups[compiler] = append(groups[compiler], alias)
	}
	type entry struct {
		compiler string
		aliases  []string
	}
	entries := make([]entry, 0, len(groups))
	for c, a := range groups {
		sort.Strings(a)
		entries = append(entries, entry{compiler: c, aliases: a})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].aliases[0] < entries[j].aliases[0]
	})
	var sb strings.Builder
	sb.WriteString("supported languages:\n")
	for _, e := range entries {
		fmt.Fprintf(&sb, "- %s (%s)\n", strings.Join(e.aliases, ", "), e.compiler)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// parseRunCommand parses content and returns lang and code if it has the form `/run <lang>\n<code>`.
func parseRunCommand(content string) (lang, code string, ok bool) {
	if !strings.HasPrefix(content, "/run ") {
		return "", "", false
	}
	rest := strings.TrimPrefix(content, "/run ")
	nl := strings.IndexByte(rest, '\n')
	if nl < 0 {
		return "", "", false
	}
	lang = strings.TrimSpace(rest[:nl])
	code = strings.TrimLeft(rest[nl+1:], "\n")
	if lang == "" || code == "" {
		return "", "", false
	}
	return lang, code, true
}

func runWandbox(ctx context.Context, compiler, code string) (string, error) {
	body, err := json.Marshal(wandboxRequest{
		Code:     code,
		Compiler: compiler,
		Save:     false,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://wandbox.org/api/compile.json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("wandbox: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	var wr wandboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
		return "", err
	}

	var sb strings.Builder
	for _, s := range []string{wr.CompilerOutput, wr.CompilerError, wr.ProgramOutput, wr.ProgramError} {
		if s == "" {
			continue
		}
		sb.WriteString(s)
		if !strings.HasSuffix(s, "\n") {
			sb.WriteByte('\n')
		}
	}
	if wr.Signal != "" {
		fmt.Fprintf(&sb, "signal: %s\n", wr.Signal)
	}
	if sb.Len() == 0 {
		return "(no output)", nil
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

func main() {
	var ver bool
	flag.BoolVar(&ver, "version", false, "show version")
	flag.Parse()

	if ver {
		fmt.Printf("%s %s (rev: %s)\n", name, version, revision)
		os.Exit(0)
	}

	nsec := os.Getenv("BOT_NSEC")
	if nsec == "" {
		log.Fatal("BOT_NSEC is required")
	}
	prefix, value, err := nip19.Decode(nsec)
	if err != nil {
		log.Fatalf("invalid BOT_NSEC: %v", err)
	}
	if prefix != "nsec" {
		log.Fatalf("BOT_NSEC must be nsec, got %q", prefix)
	}
	secretKey := value.(string)

	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	e.POST("/", func(c echo.Context) error {
		var ev nostr.Event
		if err := json.NewDecoder(io.TeeReader(c.Request().Body, os.Stdout)).Decode(&ev); err != nil {
			return c.JSON(http.StatusBadRequest, echo.Map{"error": "invalid event json: " + err.Error()})
		}

		if ok, err := ev.CheckSignature(); err != nil || !ok {
			return c.JSON(http.StatusBadRequest, echo.Map{"error": "invalid signature"})
		}

		var output string
		if strings.TrimSpace(ev.Content) == "/run list" {
			output = listLanguages()
		} else {
			lang, code, ok := parseRunCommand(ev.Content)
			if !ok {
				return c.JSON(http.StatusBadRequest, echo.Map{"error": "not a /run command"})
			}

			compiler, ok := langToCompiler[strings.ToLower(lang)]
			if !ok {
				return c.JSON(http.StatusBadRequest, echo.Map{"error": "unsupported language: " + lang})
			}

			ctx, cancel := context.WithTimeout(c.Request().Context(), 30*time.Second)
			defer cancel()

			out, err := runWandbox(ctx, compiler, code)
			if err != nil {
				return c.JSON(http.StatusBadGateway, echo.Map{"error": err.Error()})
			}
			output = out
		}

		reply := nostr.Event{
			Kind:      nostr.KindTextNote,
			CreatedAt: nostr.Now(),
			Tags: nostr.Tags{
				nostr.Tag{"e", ev.ID, "", "reply"},
				nostr.Tag{"p", ev.PubKey},
			},
			Content: output,
		}
		if err := reply.Sign(secretKey); err != nil {
			return c.JSON(http.StatusInternalServerError, echo.Map{"error": err.Error()})
		}
		return c.JSON(http.StatusOK, reply)
	})

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Printf("listening on %s", addr)
	if err := e.Start(addr); err != nil {
		log.Fatal(err)
	}
}
