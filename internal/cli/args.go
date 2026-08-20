package cli

import "flag"

// parseArgs parses a flag set, tolerating flags written after the positional
// arguments.
//
// Go's flag package stops at the first non-flag argument, so
//
//	plum note "why I did it" -rejected "the thing I didn't"
//
// silently records the flag as part of the prose rather than as a rejected
// alternative. Nothing errors and nothing looks wrong — the note is simply
// missing the half that mattered, which is precisely the loss the command
// exists to prevent.
//
// Rather than teach every user that flags come first, the arguments are
// reordered before parsing.
func parseArgs(fs *flag.FlagSet, args []string) error {
	return fs.Parse(hoistFlags(fs, args))
}

// hoistFlags moves recognised flags ahead of the positionals.
//
// Only flags this command actually defines are moved. A positional that merely
// begins with a dash — a rationale that opens with "-- but" — is left alone, and
// everything after a literal "--" is untouched, because `plum run -- claude
// -flag` must hand those through to the agent rather than eat them.
func hoistFlags(fs *flag.FlagSet, args []string) []string {
	known := map[string]*flag.Flag{}
	fs.VisitAll(func(f *flag.Flag) { known[f.Name] = f })

	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i:]...)
			break
		}
		name, hasValue := flagName(a)
		f, ok := known[name]
		if !ok {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		// A non-boolean flag written as "-rejected value" takes the next
		// argument with it; "-rejected=value" already carries its own.
		if !hasValue && !isBoolFlag(f) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

// flagName strips the leading dashes and any "=value", reporting whether the
// value travelled with the flag.
func flagName(a string) (string, bool) {
	if len(a) < 2 || a[0] != '-' {
		return "", false
	}
	a = a[1:]
	if len(a) > 0 && a[0] == '-' {
		a = a[1:]
	}
	for i := 0; i < len(a); i++ {
		if a[i] == '=' {
			return a[:i], true
		}
	}
	return a, false
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}
