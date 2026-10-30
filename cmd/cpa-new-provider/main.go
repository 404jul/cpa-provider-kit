// Command cpa-new-provider scaffolds a CLIProxyAPI provider plugin repository
// built on cpa-provider-kit.
//
//	go run github.com/404jul/cpa-provider-kit/cmd/cpa-new-provider \
//	  -id minimax -name MiniMax -owner 404jul \
//	  -base-url https://api.minimax.io -chat /v1/text/chatcompletion_v2 \
//	  -anthropic /anthropic/v1/messages -models MiniMax-M2 -key-url https://…
package main

import (
	"bytes"
	"embed"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode"
)

const kitModule = "github.com/404jul/cpa-provider-kit"

//go:embed all:template
var templates embed.FS

// Params are the template inputs.
type Params struct {
	ID, Name, Module, Author, Repository                    string
	BaseURL, KeyURL, ValidatePath                           string
	ChatPath, AnthropicPath, ResponsesPath, CountTokensPath string
	Models                                                  []string
	KitModule, KitVersion, KitReplace                       string
	Year                                                    int
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cpa-new-provider:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("cpa-new-provider", flag.ContinueOnError)
	var p Params
	var owner, models, out, kitDir string
	var tidy bool
	fs.StringVar(&p.ID, "id", "", "provider/plugin ID, e.g. minimax (required)")
	fs.StringVar(&p.Name, "name", "", "display name (default: ID title-cased)")
	fs.StringVar(&owner, "owner", "", "GitHub owner; sets -module and -repo to github.com/<owner>/cpa-plugin-<id>")
	fs.StringVar(&p.Module, "module", "", "Go module path")
	fs.StringVar(&p.Repository, "repo", "", "GitHub repository URL shown in CPA")
	fs.StringVar(&p.Author, "author", "", "author shown in CPA and LICENSE (default: owner)")
	fs.StringVar(&p.BaseURL, "base-url", "", "API origin, e.g. https://api.example.com (required)")
	fs.StringVar(&p.ChatPath, "chat", "", "OpenAI Chat Completions path, e.g. /v1/chat/completions")
	fs.StringVar(&p.AnthropicPath, "anthropic", "", "Anthropic Messages path, e.g. /anthropic/v1/messages")
	fs.StringVar(&p.CountTokensPath, "count-tokens", "", "Anthropic count_tokens path")
	fs.StringVar(&p.ResponsesPath, "responses", "", "OpenAI Responses path, e.g. /v1/responses")
	fs.StringVar(&models, "models", "", "comma-separated model IDs (required)")
	fs.StringVar(&p.KeyURL, "key-url", "", "page where users create API keys")
	fs.StringVar(&p.ValidatePath, "validate", "", "GET path used to check submitted keys, e.g. /v1/models")
	fs.StringVar(&out, "out", "", "output directory (default: ./cpa-plugin-<id>)")
	fs.StringVar(&kitDir, "kit-dir", "", "local cpa-provider-kit checkout to use via a go.mod replace")
	fs.StringVar(&p.KitVersion, "kit-version", "v0.1.0", "cpa-provider-kit version to require")
	fs.BoolVar(&tidy, "tidy", true, "run go mod tidy in the new repository")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if !idPattern.MatchString(p.ID) {
		return fmt.Errorf("-id must match %s", idPattern)
	}
	if owner != "" {
		if p.Module == "" {
			p.Module = "github.com/" + owner + "/cpa-plugin-" + p.ID
		}
		if p.Repository == "" {
			p.Repository = "https://github.com/" + owner + "/cpa-plugin-" + p.ID
		}
		if p.Author == "" {
			p.Author = owner
		}
	}
	switch {
	case p.Module == "" || p.Repository == "" || p.Author == "":
		return errors.New("set -owner, or all of -module, -repo, and -author")
	case p.BaseURL == "":
		return errors.New("-base-url is required")
	case p.ChatPath == "" && p.AnthropicPath == "" && p.ResponsesPath == "":
		return errors.New("set at least one of -chat, -anthropic, -responses")
	case p.CountTokensPath != "" && p.AnthropicPath == "":
		return errors.New("-count-tokens requires -anthropic")
	}
	if u, err := url.Parse(p.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("-base-url must be an absolute http(s) URL")
	}
	p.BaseURL = strings.TrimRight(p.BaseURL, "/")
	for _, route := range []string{p.ChatPath, p.AnthropicPath, p.ResponsesPath, p.CountTokensPath, p.ValidatePath} {
		if route != "" && !strings.HasPrefix(route, "/") {
			return fmt.Errorf("path %q must start with /", route)
		}
	}
	for _, model := range strings.Split(models, ",") {
		if model = strings.TrimSpace(model); model != "" {
			p.Models = append(p.Models, model)
		}
	}
	if len(p.Models) == 0 {
		return errors.New("-models is required")
	}
	if p.Name == "" {
		p.Name = titleCase(p.ID)
	}
	if out == "" {
		out = "cpa-plugin-" + p.ID
	}
	p.KitModule = kitModule
	p.Year = time.Now().Year()

	outAbs, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	if entries, errRead := os.ReadDir(outAbs); errRead == nil && len(entries) > 0 {
		return fmt.Errorf("%s already exists and is not empty", out)
	}
	if kitDir != "" {
		kitAbs, errKit := filepath.Abs(kitDir)
		if errKit != nil {
			return errKit
		}
		if _, errStat := os.Stat(filepath.Join(kitAbs, "kit", "spec.go")); errStat != nil {
			return fmt.Errorf("-kit-dir %s is not a cpa-provider-kit checkout", kitDir)
		}
		rel, errRel := filepath.Rel(outAbs, kitAbs)
		if errRel != nil {
			rel = kitAbs
		}
		p.KitReplace = filepath.ToSlash(rel)
	}

	if err := render(outAbs, p); err != nil {
		return err
	}
	if tidy {
		cmd := exec.Command("go", "mod", "tidy")
		cmd.Dir = outAbs
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("go mod tidy: %w", err)
		}
	}
	fmt.Printf("Created %s\n\nNext:\n  cd %s\n  make test && make build\n", out, out)
	if p.KitReplace == "" {
		fmt.Printf("  (requires %s@%s to be published; use -kit-dir for a local checkout)\n", kitModule, p.KitVersion)
	}
	return nil
}

func render(outDir string, p Params) error {
	funcs := template.FuncMap{"quote": strconv.Quote}
	return fs.WalkDir(templates, "template", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		raw, errRead := templates.ReadFile(name)
		if errRead != nil {
			return errRead
		}
		tmpl, errParse := template.New(name).Delims("[[", "]]").Funcs(funcs).Option("missingkey=error").Parse(string(raw))
		if errParse != nil {
			return fmt.Errorf("%s: %w", name, errParse)
		}
		var buf bytes.Buffer
		if errExec := tmpl.Execute(&buf, p); errExec != nil {
			return fmt.Errorf("%s: %w", name, errExec)
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(name, "template/"), ".tmpl")
		if path.Base(rel) == "gitignore" {
			rel = path.Join(path.Dir(rel), ".gitignore")
		}
		target := filepath.Join(outDir, filepath.FromSlash(rel))
		if errMkdir := os.MkdirAll(filepath.Dir(target), 0o755); errMkdir != nil {
			return errMkdir
		}
		content := buf.Bytes()
		if strings.HasSuffix(rel, ".go") {
			formatted, errFormat := format.Source(content)
			if errFormat != nil {
				return fmt.Errorf("%s: generated code does not parse: %w", rel, errFormat)
			}
			content = formatted
		}
		return os.WriteFile(target, content, 0o644)
	})
}

func titleCase(id string) string {
	words := strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	for i, word := range words {
		runes := []rune(word)
		runes[0] = unicode.ToUpper(runes[0])
		words[i] = string(runes)
	}
	return strings.Join(words, " ")
}
