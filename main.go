package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
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

const version = "0.0.5"

var revision = "HEAD"

// langToCompiler maps the `lang` part of `/run <lang>` to a paiza.io language name.
// The up-to-date list is available at https://api.paiza.io/runners/get_languages.
var langToCompiler = map[string]string{
	"rb":     "ruby",
	"ruby":   "ruby",
	"py":     "python3",
	"python": "python3",
	"go":     "go",
	"js":     "javascript",
	"node":   "javascript",
	"c":      "c",
	"cpp":    "cpp",
	"c++":    "cpp",
	"rs":     "rust",
	"rust":   "rust",
	"sh":     "bash",
	"bash":   "bash",
	"php":    "php",
	"pl":     "perl",
	"perl":   "perl",
	"swift":  "swift",
}

type paizaCreateResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

type paizaDetailsResponse struct {
	Status        string `json:"status"`
	BuildStdout   string `json:"build_stdout"`
	BuildStderr   string `json:"build_stderr"`
	BuildResult   string `json:"build_result"`
	Stdout        string `json:"stdout"`
	Stderr        string `json:"stderr"`
	Result        string `json:"result"`
	ExitCode      int    `json:"exit_code"`
	BuildExitCode int    `json:"build_exit_code"`
	Error         string `json:"error"`
}

// defaultFetchRelays is used when the RELAYS environment variable is not set.
// It is used by /rerun to look up the original /run event from relays.
var defaultFetchRelays = []string{
	"wss://relay-jp.nostr.wirednet.jp",
	"wss://yabu.me",
	"wss://relay.damus.io",
}

var fetchRelays []string

// fetchOriginalRunEvent looks up the event referenced by the "root" e-tag of ev
// (typically the original /run command the user replied to via the bot's output).
func fetchOriginalRunEvent(ctx context.Context, ev *nostr.Event) (*nostr.Event, error) {
	var rootID, rootHint string
	for _, t := range ev.Tags {
		if len(t) >= 4 && t[0] == "e" && t[3] == "root" && t[1] != "" {
			rootID = t[1]
			if len(t) >= 3 {
				rootHint = t[2]
			}
			break
		}
	}
	if rootID == "" {
		return nil, fmt.Errorf("/rerun: no root e-tag found")
	}

	relays := map[string]struct{}{}
	for _, r := range fetchRelays {
		relays[r] = struct{}{}
	}
	if rootHint != "" {
		relays[rootHint] = struct{}{}
	}
	if len(relays) == 0 {
		return nil, fmt.Errorf("no relays configured")
	}
	urls := make([]string, 0, len(relays))
	for r := range relays {
		urls = append(urls, r)
	}

	pool := nostr.NewSimplePool(ctx)
	defer pool.Close("done")
	res := pool.QuerySingle(ctx, urls, nostr.Filter{IDs: []string{rootID}, Limit: 1})
	if res == nil || res.Event == nil {
		return nil, fmt.Errorf("/rerun: root event not found: %s", rootID)
	}
	if _, _, ok := parseRunCommand(res.Event.Content); !ok {
		return nil, fmt.Errorf("/rerun: root event is not a /run command")
	}
	return res.Event, nil
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

// buildReplyTags builds NIP-10/NIP-28 compliant tags for a reply to ev.
// For channel messages, the incoming event's "root" e-tag (pointing to the kind:40 channel creation
// event) is preserved so the reply stays in the same channel.
func buildReplyTags(ev *nostr.Event) nostr.Tags {
	var tags nostr.Tags
	if ev.Kind == nostr.KindChannelMessage {
		for _, t := range ev.Tags {
			if len(t) >= 4 && t[0] == "e" && t[3] == "root" {
				tags = append(tags, t)
				break
			}
		}
	}
	tags = append(tags, nostr.Tag{"e", ev.ID, "", "reply"})
	tags = append(tags, nostr.Tag{"p", ev.PubKey})
	return tags
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

func runPaiza(ctx context.Context, language, code string) (string, error) {
	form := url.Values{}
	form.Set("source_code", code)
	form.Set("language", language)
	form.Set("input", "")
	form.Set("longpoll", "true")
	form.Set("longpoll_timeout", "20")
	form.Set("api_key", "guest")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.paiza.io/runners/create", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("paiza: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	var cr paizaCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", err
	}
	if cr.Error != "" {
		return "", fmt.Errorf("paiza: %s", cr.Error)
	}
	if cr.ID == "" {
		return "", fmt.Errorf("paiza: empty runner id")
	}

	for cr.Status != "completed" {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		statusURL := fmt.Sprintf("https://api.paiza.io/runners/get_status?id=%s&api_key=guest",
			url.QueryEscape(cr.ID))
		sreq, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
		if err != nil {
			return "", err
		}
		sresp, err := http.DefaultClient.Do(sreq)
		if err != nil {
			return "", err
		}
		var sr paizaCreateResponse
		err = json.NewDecoder(sresp.Body).Decode(&sr)
		sresp.Body.Close()
		if err != nil {
			return "", err
		}
		cr.Status = sr.Status
		if cr.Status != "completed" {
			time.Sleep(500 * time.Millisecond)
		}
	}

	detailsURL := fmt.Sprintf("https://api.paiza.io/runners/get_details?id=%s&api_key=guest",
		url.QueryEscape(cr.ID))
	dreq, err := http.NewRequestWithContext(ctx, http.MethodGet, detailsURL, nil)
	if err != nil {
		return "", err
	}
	dresp, err := http.DefaultClient.Do(dreq)
	if err != nil {
		return "", err
	}
	defer dresp.Body.Close()

	var dr paizaDetailsResponse
	if err := json.NewDecoder(dresp.Body).Decode(&dr); err != nil {
		return "", err
	}
	if dr.Error != "" {
		return "", fmt.Errorf("paiza: %s", dr.Error)
	}

	var sb strings.Builder
	for _, s := range []string{dr.BuildStdout, dr.BuildStderr, dr.Stdout, dr.Stderr} {
		if s == "" {
			continue
		}
		sb.WriteString(s)
		if !strings.HasSuffix(s, "\n") {
			sb.WriteByte('\n')
		}
	}
	if dr.BuildResult != "" && dr.BuildResult != "success" {
		fmt.Fprintf(&sb, "build: %s\n", dr.BuildResult)
	}
	if dr.Result != "" && dr.Result != "success" {
		fmt.Fprintf(&sb, "result: %s\n", dr.Result)
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

	if rs := os.Getenv("RELAYS"); rs != "" {
		for _, r := range strings.Split(rs, ",") {
			if r = strings.TrimSpace(r); r != "" {
				fetchRelays = append(fetchRelays, r)
			}
		}
	}
	if len(fetchRelays) == 0 {
		fetchRelays = defaultFetchRelays
	}

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

		if ev.Kind != nostr.KindTextNote && ev.Kind != nostr.KindChannelMessage {
			return c.JSON(http.StatusBadRequest, echo.Map{"error": "unsupported kind"})
		}

		var output string
		var runLang, runCode string
		switch {
		case strings.TrimSpace(ev.Content) == "/run list":
			output = listLanguages()
		case strings.TrimSpace(ev.Content) == "/rerun":
			fetchCtx, cancel := context.WithTimeout(c.Request().Context(), 15*time.Second)
			orig, err := fetchOriginalRunEvent(fetchCtx, &ev)
			cancel()
			if err != nil {
				return c.JSON(http.StatusBadRequest, echo.Map{"error": err.Error()})
			}
			runLang, runCode, _ = parseRunCommand(orig.Content)
		default:
			var ok bool
			runLang, runCode, ok = parseRunCommand(ev.Content)
			if !ok {
				return c.JSON(http.StatusBadRequest, echo.Map{"error": "not a /run command"})
			}
		}

		if runLang != "" {
			compiler, ok := langToCompiler[strings.ToLower(runLang)]
			if !ok {
				return c.JSON(http.StatusBadRequest, echo.Map{"error": "unsupported language: " + runLang})
			}
			ctx, cancel := context.WithTimeout(c.Request().Context(), 30*time.Second)
			defer cancel()
			out, err := runPaiza(ctx, compiler, runCode)
			if err != nil {
				return c.JSON(http.StatusBadGateway, echo.Map{"error": err.Error()})
			}
			output = out
		}

		reply := nostr.Event{
			Kind:      ev.Kind,
			CreatedAt: nostr.Now(),
			Tags:      buildReplyTags(&ev),
			Content:   output,
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
