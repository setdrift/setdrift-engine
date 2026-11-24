// tests that docs/commands has a page for every command and that each page matches it, every flag with its type, default and relations, every example a command line that parses
// go test -run TestCommandPages -update (make docs) writes what can be written from the code, a page for a new command, a section for a new flag, a changed type line, and marks the rest to write by hand

package main

import (
	"bufio"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var update = flag.Bool("update", false, "write the command pages' sections for new flags, their type lines, and pages for new commands")

const stub = "<!-- write this -->"

var pagesDir = filepath.Join("..", "..", "docs", "commands")

var flagHeading = regexp.MustCompile("^(###|####) `(--[a-z0-9-]+)`(?:, `-([a-z])`)?$")

func documented() []*cobra.Command {
	var out []*cobra.Command
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if sub.Hidden || sub.Name() == "help" || c.Name() == "completion" {
				continue
			}
			out = append(out, sub)
			walk(sub)
		}
	}
	root := newRootCmd()
	root.InitDefaultCompletionCmd()
	walk(root)
	for _, c := range out {
		c.InitDefaultHelpFlag()
	}
	return out
}

func pageName(c *cobra.Command) string {
	return strings.Join(strings.Fields(c.CommandPath())[1:], "-") + ".md"
}

func flagType(f *pflag.Flag) string {
	switch f.Value.Type() {
	case "string":
		return "text"
	case "stringSlice", "stringArray":
		return "list"
	case "bool":
		return "on/off"
	case "int", "int32", "int64", "float64":
		return "number"
	}
	return f.Value.Type()
}

func flagDefault(f *pflag.Flag) string {
	switch f.DefValue {
	case "", "[]":
		return "none"
	case "false":
		return "off"
	case "true":
		return "on"
	}
	return "`" + f.DefValue + "`"
}

func parentOf(f *pflag.Flag) string {
	if p := f.Annotations[parentKey]; len(p) == 1 {
		return p[0]
	}
	return ""
}

func typeLine(f *pflag.Flag) string {
	line := "*" + flagType(f) + " · default " + flagDefault(f)
	if len(f.Annotations[cobra.BashCompOneRequiredFlag]) > 0 {
		line = "*" + flagType(f) + " · required"
	}
	if len(f.Annotations[needsParentKey]) > 0 {
		line += " · needs `--" + parentOf(f) + "`"
	}
	if others := slices.Sorted(slices.Values(f.Annotations[conflictsKey])); len(others) > 0 {
		line += " · not with `--" + strings.Join(others, "`, `--") + "`"
	}
	return line + "*"
}

func heading(f *pflag.Flag) string {
	level := "###"
	if parentOf(f) != "" {
		level = "####"
	}
	h := level + " `--" + f.Name + "`"
	if f.Shorthand != "" {
		h += ", `-" + f.Shorthand + "`"
	}
	return h
}

func flagsOf(c *cobra.Command) []*pflag.Flag {
	var out []*pflag.Flag
	c.Flags().VisitAll(func(f *pflag.Flag) { out = append(out, f) })
	return out
}

func section(f *pflag.Flag) string {
	return "\n" + heading(f) + "\n\n" + typeLine(f) + "\n\n" + stub + "\n"
}

func skeleton(c *cobra.Command) string {
	var b strings.Builder
	b.WriteString("# " + c.CommandPath() + "\n\n" + stub + "\n\n## Synopsis\n\n```sh\n" + c.UseLine() + "\n```\n\n## Examples\n\n" + stub + "\n\n## Flags\n")
	for _, f := range flagsOf(c) {
		if parentOf(f) != "" {
			continue
		}
		b.WriteString(section(f))
		for _, child := range flagsOf(c) {
			if parentOf(child) == f.Name {
				b.WriteString(section(child))
			}
		}
	}
	b.WriteString("\n## See also\n\n" + stub + "\n")
	return b.String()
}

func sectionEnd(lines []string, start int, sibling string) int {
	for j := start + 1; j < len(lines); j++ {
		if strings.HasPrefix(lines[j], "## ") || sibling != "" && strings.HasPrefix(lines[j], sibling) {
			for j > start+1 && lines[j-1] == "" {
				j--
			}
			return j
		}
	}
	return len(lines)
}

func updatePage(t *testing.T, c *cobra.Command, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := os.WriteFile(path, []byte(skeleton(c)), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	find := func(name string) int {
		return slices.IndexFunc(lines, func(l string) bool {
			m := flagHeading.FindStringSubmatch(l)
			return m != nil && m[2] == "--"+name
		})
	}
	for _, f := range flagsOf(c) {
		if i := find(f.Name); i >= 0 {
			lines[i] = heading(f)
			if i+2 < len(lines) && strings.HasPrefix(lines[i+2], "*") {
				lines[i+2] = typeLine(f)
			}
			continue
		}
		at := -1
		if i := slices.Index(lines, "## Flags"); i >= 0 {
			at = sectionEnd(lines, i, "")
		}
		if p := find(parentOf(f)); parentOf(f) != "" && p >= 0 {
			at = sectionEnd(lines, p, "### ")
		}
		if at < 0 {
			lines = append(lines, "", "## Flags")
			at = len(lines)
		}
		lines = slices.Insert(lines, at, strings.Split(strings.TrimSuffix(section(f), "\n"), "\n")...)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

// check that every command has a page, every flag its section at the right level with the right type line, every section a real flag, and nothing left to write
func TestCommandPagesFlags(t *testing.T) {
	for _, c := range documented() {
		path := filepath.Join(pagesDir, pageName(c))
		if *update {
			updatePage(t, c, path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s has no page: %v (make docs writes one to fill in)", c.CommandPath(), err)
			continue
		}
		page, name := string(b), pageName(c)
		if strings.Contains(page, stub) {
			t.Errorf("%s still has parts to write, marked %s", name, stub)
		}
		if !strings.Contains(page, "```sh\n"+c.UseLine()+"\n```") {
			t.Errorf("%s's synopsis is not %q", name, c.UseLine())
		}
		for _, sub := range c.Commands() {
			if !sub.Hidden && sub.Name() != "help" && c.Name() != "completion" && !strings.Contains(page, "]("+pageName(sub)+")") {
				t.Errorf("%s does not link %s", name, pageName(sub))
			}
		}
		// invoking checkFlagSections to check each flag section on the page, its defined below
		checkFlagSections(t, c, name, page)
	}
}

// checkFlagSections takes a command and its page and checks every flag section is a real flag, at the right level, with the right heading and type line, and every flag has a section
func checkFlagSections(t *testing.T, c *cobra.Command, name, page string) {
	lines := strings.Split(page, "\n")
	seen, parentSection := map[string]bool{}, ""
	for i, line := range lines {
		m := flagHeading.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		fname := strings.TrimPrefix(m[2], "--")
		if m[1] == "###" {
			parentSection = fname
		}
		f := c.Flags().Lookup(fname)
		if f == nil {
			t.Errorf("%s documents %s, which %s does not have", name, m[2], c.CommandPath())
			continue
		}
		seen[fname] = true
		if line != heading(f) {
			t.Errorf("%s: %q should read %q", name, line, heading(f))
		}
		if p := parentOf(f); p != "" && parentSection != p {
			t.Errorf("%s: --%s belongs under --%s's section", name, fname, p)
		}
		if i+2 >= len(lines) || lines[i+2] != typeLine(f) {
			t.Errorf("%s: under %s, want the line %q", name, m[2], typeLine(f))
		}
	}
	for _, f := range flagsOf(c) {
		if !seen[f.Name] {
			t.Errorf("%s has no section for --%s (make docs adds one to fill in)", name, f.Name)
		}
	}
}

func examples(page string) []string {
	var out []string
	inSh, cont := false, ""
	sc := bufio.NewScanner(strings.NewReader(page))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "```sh":
			inSh = true
		case line == "```":
			inSh = false
		case inSh:
			if strings.HasSuffix(line, `\`) {
				cont += strings.TrimSuffix(line, `\`) + " "
				continue
			}
			line, cont = cont+line, ""
			if strings.Contains(line, "setdrift ") {
				out = append(out, line)
			}
		}
	}
	return out
}

func commandLine(line string) []string {
	for _, stop := range []string{" #", " |", " >", " <", " ||", " &&"} {
		if i := strings.Index(line, stop); i >= 0 {
			line = line[:i]
		}
	}
	fields := strings.Fields(line)
	for len(fields) > 0 && fields[0] != "setdrift" {
		fields = fields[1:]
	}
	if len(fields) > 0 {
		fields = fields[1:]
	}
	return fields
}

// check that every example on each page parses as its command with its flags, their relations and its arguments, and that every flag but help is in an example
func TestCommandPagesExamples(t *testing.T) {
	for _, c := range documented() {
		b, err := os.ReadFile(filepath.Join(pagesDir, pageName(c)))
		if err != nil {
			continue
		}
		used := map[string]bool{}
		for _, line := range examples(string(b)) {
			if line == c.UseLine() {
				continue
			}
			root := newRootCmd()
			root.InitDefaultCompletionCmd()
			cmd, rest, err := root.Find(commandLine(line))
			if err != nil {
				t.Errorf("%s: %q: %v", pageName(c), line, err)
				continue
			}
			if cmd.CommandPath() != c.CommandPath() {
				continue
			}
			cmd.InitDefaultHelpFlag()
			if err := cmd.ParseFlags(rest); err != nil {
				t.Errorf("%s: %q: %v", pageName(c), line, err)
				continue
			}
			if err := checkFlags(cmd); err != nil {
				t.Errorf("%s: %q: %v", pageName(c), line, err)
			}
			if cmd.Args != nil {
				if err := cmd.Args(cmd, cmd.Flags().Args()); err != nil {
					t.Errorf("%s: %q: %v", pageName(c), line, err)
				}
			}
			cmd.Flags().Visit(func(f *pflag.Flag) { used[f.Name] = true })
		}
		for _, f := range flagsOf(c) {
			if f.Name != "help" && !used[f.Name] {
				t.Errorf("%s has no example using --%s", pageName(c), f.Name)
			}
		}
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
