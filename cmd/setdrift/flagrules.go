// declares how flags relate, a flag that belongs under another and a flag that cant be used with another, and checks them before any command runs
// the relations live on the flags as annotations, so the cli enforces them in one place and the docs test checks the command pages show them

package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// the annotations a flag can carry, read by checkFlags below and by docs_test.go
const (
	// parentKey names the flag this one belongs under, its section goes inside the parent's on the command page
	parentKey = "setdrift.io/parent"
	// needsParentKey marks a flag that does nothing without its parent, so giving it alone is an error
	needsParentKey = "setdrift.io/needs-parent"
	// conflictsKey lists the flags this one cant be used with
	conflictsKey = "setdrift.io/conflicts"
	// reasonKey says why it cant, the start of the error, like "applies to reading a cluster"
	reasonKey = "setdrift.io/conflicts-reason"
)

// belongsUnder takes a flag set, a flag and its parent and records that the flag belongs under the parent
// needed tells if the flag does nothing without its parent, then giving it alone is an error
func belongsUnder(fs *pflag.FlagSet, flag, parent string, needed bool) {
	// invoking SetAnnotation to record the parent, a missing flag is a mistake in the code so it panics in every test
	if err := fs.SetAnnotation(flag, parentKey, []string{parent}); err != nil {
		panic(err)
	}
	// checking if the flag needs its parent, if so thats recorded too
	if needed {
		if err := fs.SetAnnotation(flag, needsParentKey, []string{"true"}); err != nil {
			panic(err)
		}
	}
}

// cannotUseWith takes a flag set, why a flag cant be used with others, the flag and the others, and records it
// reason follows the flag in the error, like applies to reading a cluster in "--aws applies to reading a cluster, so it cannot be used with --from-dir"
func cannotUseWith(fs *pflag.FlagSet, reason, flag string, others ...string) {
	// invoking SetAnnotation to record both, a missing flag is a mistake in the code so it panics in every test
	if err := fs.SetAnnotation(flag, conflictsKey, others); err != nil {
		panic(err)
	}
	if err := fs.SetAnnotation(flag, reasonKey, []string{reason}); err != nil {
		panic(err)
	}
}

// checkFlags takes a command and checks the flags it was given against their relations, before it runs
// a flag given without the parent it needs, or with one it cant be used with, is an error rather than being ignored
func checkFlags(cmd *cobra.Command) error {
	fs := cmd.Flags()
	var err error
	// going through the flags in name order, so the first error is the same every time
	fs.VisitAll(func(f *pflag.Flag) {
		// checking if an error was already found or this flag wasnt given
		if err != nil || !f.Changed {
			return
		}
		var given []string
		// going through the flags this one cant be used with, keeping those that were given
		for _, other := range f.Annotations[conflictsKey] {
			// checking if this one was given
			if fs.Changed(other) {
				given = append(given, display(fs.Lookup(other)))
			}
		}
		// checking if any were given, if so the error names this flag, the reason and each of them
		// a conflict is checked first, its the bigger mistake, like a cluster flag given with --from-dir
		if len(given) > 0 {
			// invoking display to name the flags, its defined below
			err = fmt.Errorf("%s %s, so it cannot be used with %s", display(f), f.Annotations[reasonKey][0], strings.Join(given, " or "))
			return
		}
		// checking if the flag needs its parent and the parent wasnt given
		if parent := f.Annotations[parentKey]; len(parent) == 1 && len(f.Annotations[needsParentKey]) > 0 && !fs.Changed(parent[0]) {
			err = fmt.Errorf("%s is for %s, so it cannot be used without it", display(f), display(fs.Lookup(parent[0])))
			return
		}
	})
	return err
}

// display gives back how an error names a flag, --name, with its short form after it when it has one, like --namespace (-n)
func display(f *pflag.Flag) string {
	// checking if the flag has a short form
	if f.Shorthand == "" {
		return "--" + f.Name
	}
	return "--" + f.Name + " (-" + f.Shorthand + ")"
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
