// Command docgen writes the settings reference, docs/guide/settings.md, from the settings
// internal/config registers, so the page cannot drift from the daemon.
//
//	go run ./internal/config/docgen -o docs/guide/settings.md      write it
//	go run ./internal/config/docgen -check docs/guide/settings.md  fail if it differs
//
// `make settings-doc` and `make settings-doc-check` run these.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/TurtleByte-IN/mailrules/internal/config"
)

const header = `---
title: Settings reference
description: Every MailRules setting, as an environment variable and as a command-line flag, with its default.
order: 110
---

Every setting is an environment variable, and also a flag of ` + "`mailrules serve`" + `: the flag wins when both are set. The flag is the variable's name in lower case, without ` + "`MAILRULES_`" + `, with ` + "`-`" + ` for ` + "`_`" + `. Give keys and passwords as environment variables, not flags: other users of the machine can see a program's flags in the process list.

The decision models, their thresholds and the provider keys can also be set in the browser, under Settings. What is saved there wins over the environment; see [Decision models and keys](./models.md). Dry-run works the same way: see [Dry-run and going live](./dry-run.md).

Where to put the variables depends on how you run MailRules; see [Install](./install.md). This page is generated from the daemon's source, so it matches the release it comes with.
`

func main() {
	out := flag.String("o", "", "write the reference to this file")
	check := flag.String("check", "", "fail if this file differs from the reference")
	flag.Parse()
	if (*out == "") == (*check == "") {
		fmt.Fprintln(os.Stderr, "usage: docgen -o FILE | -check FILE")
		os.Exit(2)
	}
	want := render(config.Settings())
	if *out != "" {
		if err := os.WriteFile(*out, want, 0o644); err != nil { // #nosec G306 -- a documentation file in the repository
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	got, err := os.ReadFile(*check) // #nosec G304 -- the path comes from the Makefile
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !bytes.Equal(got, want) {
		fmt.Fprintf(os.Stderr, "%s is out of date with internal/config: run `make settings-doc` and commit the result; do not edit it by hand\n", *check)
		os.Exit(1)
	}
}

// render lists the settings as one table per group, in the order they are registered.
func render(settings []config.Setting) []byte {
	var b bytes.Buffer
	b.WriteString(header)
	group := ""
	for _, s := range settings {
		if s.Group != group {
			group = s.Group
			fmt.Fprintf(&b, "\n## %s\n\n", group)
			b.WriteString("| Variable | Flag | Default | What it does |\n| --- | --- | --- | --- |\n")
		}
		def := "none"
		if s.Default != "" {
			def = code(s.Default)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", code(s.Env), code("--"+s.Flag), def, cell(s.Help))
	}
	return b.Bytes()
}

func code(s string) string { return "`" + cell(s) + "`" }

// cell escapes what would end a table cell.
func cell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
