// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Skip aix and plan9 for now: github.com/chzyer/readline doesn't support them.
// (https://golang.org/issue/32839)
//
//go:build !aix && !plan9
// +build !aix,!plan9

// The viewcore tool is a command-line tool for exploring the state of a Go process
// that has dumped core.
// Run "viewcore help" for a list of commands.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/chzyer/readline"
	"github.com/dustin/go-humanize"
	"github.com/google/pprof/profile"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/debug/internal/core"
	"golang.org/x/debug/internal/gocore"
)

// Top-level command.
var cmdRoot = &cobra.Command{
	Use:   "viewcore <corefile>",
	Short: "viewcore is a set of tools for analyzing core dumped from Go process",
	Long: `
viewcore is a set of tools for analyzing core dumped from Go process.

The following command starts an interactive shell for analysis of the
specified core.

  viewcore <corefile>

When provided a command in the following form, viewcore invokes the
command directly rather than starting in interactive mode.

  viewcore <corefile> <command>

Example:

  viewcore mycore overview

For available analysis tools, run the following command.

  viewcore help
`,
	PersistentPreRun:  func(cmd *cobra.Command, args []string) { startProfile() },
	PersistentPostRun: func(cmd *cobra.Command, args []string) { endProfile() },

	Args: cobra.ExactArgs(0), // either empty, <corefile> or help <subcommand>
	Run:  runRoot,
}

// Subcommands
var (
	cmdOverview = &cobra.Command{
		Use:   "overview",
		Short: "print a few overall statistics",
		Args:  cobra.ExactArgs(0),
		Run:   runOverview,
	}

	cmdMappings = &cobra.Command{
		Use:   "mappings",
		Short: "print virtual memory mappings",
		Args:  cobra.ExactArgs(0),
		Run:   runMappings,
	}

	cmdGoroutines = &cobra.Command{
		Use:   "goroutines",
		Short: "list goroutines",
		Args:  cobra.ExactArgs(0),
		Run:   runGoroutines,
	}

	cmdHistogram = &cobra.Command{
		Use:     "histogram",
		Aliases: []string{"histo"},
		Short:   "print histogram of heap memory use by Go type",
		Long: "print histogram of heap memory use by Go type.\n" +
			"If N is specified, it will reports only the top N buckets\n" +
			"based on the total bytes.",
		Args: cobra.ExactArgs(0),
		Run:  runHistogram,
	}

	cmdBreakdown = &cobra.Command{
		Use:   "breakdown",
		Short: "print memory use by class",
		Args:  cobra.ExactArgs(0),
		Run:   runBreakdown,
	}

	cmdObjects = &cobra.Command{
		Use:   "objects",
		Short: "print a list of all live objects",
		Args:  cobra.ExactArgs(0),
		Run:   runObjects,
	}

	cmdObjgraph = &cobra.Command{
		Use:   "objgraph <output_filename>",
		Short: "dump object graph (dot)",
		Args:  cobra.ExactArgs(1),
		Run:   runObjgraph,
	}

	cmdTypegraph = &cobra.Command{
		Use:   "typegraph <output_filename>",
		Short: "dump object graph grouped by type (dot)",
		Args:  cobra.ExactArgs(1),
		Run:   runTypegraph,
	}

	cmdPprof = &cobra.Command{
		Use:   "pprof <output_filename>",
		Short: "dump object graph to pprof format",
		Args:  cobra.ExactArgs(1),
		Run:   runPprof,
	}

	cmdReachable = &cobra.Command{
		Use:   "reachable <address>",
		Short: "find path from root to an object",
		Args:  cobra.ExactArgs(1),
		Run:   runReachable,
	}

	cmdHTML = &cobra.Command{
		Use:   "html",
		Short: "start an http server for browsing core file data on the port specified with -port",
		Args:  cobra.ExactArgs(0),
		Run:   runHTML,
	}

	cmdRead = &cobra.Command{
		Use:   "read <address> [<size>]",
		Short: "read a chunk of memory", // oh very helpful!
		Args:  cobra.RangeArgs(1, 2),
		Run:   runRead,
	}

	cmdPeek = &cobra.Command{
		Use:   "peek <type>",
		Short: "show objects that point to and are pointed from this type of object",
		Args:  cobra.ExactArgs(1),
		Run:   runPeek,
	}
)

type config struct {
	interactive bool

	// Set based on os.Args[1]
	corefile string

	// flags
	base    string
	exePath string
	cpuprof string // TODO: move to subcommand config.
}

var cfg config

func init() {
	cmdRoot.PersistentFlags().StringVar(&cfg.base, "base", "", "root directory to find core dump file references")
	cmdRoot.PersistentFlags().StringVar(&cfg.exePath, "exe", "", "main executable file")
	cmdRoot.PersistentFlags().StringVar(&cfg.cpuprof, "prof", "", "write cpu profile of viewcore to this file for viewcore's developers")

	// subcommand flags
	cmdHTML.Flags().IntP("port", "p", 8080, "port for http server")

	cmdHistogram.Flags().Int("top", 0, "reports only top N entries if N>0")
	cmdHistogram.Flags().Bool("retained", false, "reports retained size rather than flat size")

	cmdRoot.AddCommand(
		cmdOverview,
		cmdMappings,
		cmdGoroutines,
		cmdHistogram,
		cmdBreakdown,
		cmdObjects,
		cmdObjgraph,
		cmdTypegraph,
		cmdPprof,
		cmdReachable,
		cmdHTML,
		cmdRead,
		cmdPeek,
	)

	// customize the usage template - viewcore's command structure
	// is not typical of cobra-based command line tool.
	cobra.AddTemplateFunc("viewcoreUseLine", useLine)
	cmdRoot.SetUsageTemplate(usageTmpl)
}

// useLine is like cobra.Command.UseLine but tweaked to use commandPath.
func useLine(c *cobra.Command) string {
	var useline string
	if c.HasParent() {
		useline = commandPath(c.Parent()) + " " + c.Use
	} else {
		useline = c.Use
	}
	if c.DisableFlagsInUseLine {
		return useline
	}
	if c.HasAvailableFlags() && !strings.Contains(useline, "[flags]") {
		useline += " [flags]"
	}
	return useline
}

// commandPath is like cobra.Command.CommandPath but tweaked to
// use c.Use instead of c.Name for the root command so it works
// with viewcore's unusual command structure.
func commandPath(c *cobra.Command) string {
	if c.HasParent() {
		return commandPath(c) + " " + c.Name()
	}
	return c.Use
}

const usageTmpl = `Usage:{{if .Runnable}}
  {{viewcoreUseLine .}}{{end}}{{if .HasAvailableSubCommands}}
  {{viewcoreUseLine .}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}

Available Commands:{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] != "help" && !strings.HasPrefix(args[0], "-") {
		cfg.corefile = args[0]
		args = args[1:]
	}
	cmdRoot.SetArgs(args)
	cmdRoot.Execute()
}

var coreCache = &struct {
	// copy of params used to generate p.
	cfg config

	coreP   *core.Process
	gocoreP *gocore.Process
	err     error
}{}

func ResetSubCommandFlagValues(root *cobra.Command) {
	for _, c := range root.Commands() {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if f.Changed {
				f.Value.Set(f.DefValue)
				f.Changed = false
			}
		})
	}
}

// readCore reads corefile and returns core and gocore process states.
func readCore() (*core.Process, *gocore.Process, error) {
	cc := coreCache
	if cc.cfg == cfg {
		return cc.coreP, cc.gocoreP, cc.err
	}
	c, err := core.Core(cfg.corefile, cfg.base, cfg.exePath)
	if err != nil {
		return nil, nil, err
	}
	p, err := gocore.Core(c)
	if os.IsNotExist(err) && cfg.exePath == "" {
		return nil, nil, fmt.Errorf("%v; consider specifying the --exe flag", err)
	}
	if err != nil {
		return nil, nil, err
	}
	for _, w := range c.Warnings() {
		fmt.Fprintf(os.Stderr, "WARNING: %s\n", w)
	}
	cc.cfg = cfg
	cc.coreP = c
	cc.gocoreP = p
	cc.err = nil
	return c, p, nil
}

func runRoot(cmd *cobra.Command, args []string) {
	if cfg.corefile == "" {
		cmd.Usage()
		return
	}
	// Interactive mode.
	cfg.interactive = true

	p, _, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}

	// Create a dummy root to run in shell.
	root := &cobra.Command{}
	// Make all subcommands of viewcore available in the shell.
	for _, subcmd := range cmd.Commands() {
		if subcmd.Name() == "help" {
			root.SetHelpCommand(subcmd)
			continue
		}
		root.AddCommand(subcmd)
	}
	// Also, add exit command to terminate the shell.
	root.AddCommand(&cobra.Command{
		Use:     "exit",
		Aliases: []string{"quit", "bye"},
		Short:   "exit from interactive mode",
		Run: func(*cobra.Command, []string) {
			os.Exit(0)
		},
	})

	rootCompleter := readline.NewPrefixCompleter()
	for _, child := range root.Commands() {
		cmdToCompleter(rootCompleter, child)
	}

	shell, err := readline.NewEx(&readline.Config{
		Prompt:       "(viewcore) ",
		AutoComplete: rootCompleter,
		EOFPrompt:    "\n",
	})
	if err != nil {
		panic(err)
	}
	defer shell.Close()

	// nice welcome message.
	fmt.Fprintln(shell.Terminal)
	if args := p.Args(); args != "" {
		fmt.Fprintf(shell.Terminal, "Core %q was generated by %q\n", cfg.corefile, args)
	}
	fmt.Fprintf(shell.Terminal, "Entering interactive mode (type 'help' for commands)\n")

	for {
		l, err := shell.Readline()
		if err != nil {
			if err != io.EOF {
				fmt.Printf("Error: %v\n", err)
			}
			break
		}

		err = capturePanic(func() {
			ResetSubCommandFlagValues(root)
			root.SetArgs(strings.Fields(l))
			root.Execute()
		})
		if err != nil {
			fmt.Printf("Error while trying to run command %q: %v", l, err)
		}
	}
}

func capturePanic(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v\nStack: %s\n", r, debug.Stack())
		}
	}()

	fn()
	return nil
}

func cmdToCompleter(parent readline.PrefixCompleterInterface, c *cobra.Command) {
	completer := readline.PcItem(c.Name())
	parent.SetChildren(append(parent.GetChildren(), completer))
	for _, child := range c.Commands() {
		cmdToCompleter(completer, child)
	}
}

func runOverview(cmd *cobra.Command, args []string) {
	p, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}

	t := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
	fmt.Fprintf(t, "arch\t%s\n", p.Arch())
	fmt.Fprintf(t, "runtime\t%s\n", c.BuildVersion())
	var total int64
	for _, m := range p.Mappings() {
		total += m.Max().Sub(m.Min())
	}
	fmt.Fprintf(t, "memory\t%.1f MB\n", float64(total)/(1<<20))
	t.Flush()
}

func runMappings(cmd *cobra.Command, args []string) {
	p, _, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}
	t := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', tabwriter.AlignRight)
	fmt.Fprintf(t, "min\tmax\tperm\tsource\toriginal\t\n")
	for _, m := range p.Mappings() {
		perm := ""
		if m.Perm()&core.Read != 0 {
			perm += "r"
		} else {
			perm += "-"
		}
		if m.Perm()&core.Write != 0 {
			perm += "w"
		} else {
			perm += "-"
		}
		if m.Perm()&core.Exec != 0 {
			perm += "x"
		} else {
			perm += "-"
		}
		file, off := m.Source()
		fmt.Fprintf(t, "%x\t%x\t%s\t%s@%x\t", m.Min(), m.Max(), perm, file, off)
		if m.CopyOnWrite() {
			file, off = m.OrigSource()
			fmt.Fprintf(t, "%s@%x", file, off)
		}
		fmt.Fprintf(t, "\t\n")
	}
	t.Flush()
}

func runGoroutines(cmd *cobra.Command, args []string) {
	_, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}
	for _, g := range c.Goroutines() {
		fmt.Printf("G stacksize=%x\n", g.Stack())
		for _, f := range g.Frames() {
			pc := f.PC()
			entry := f.Func().Entry()
			var adj string
			switch {
			case pc == entry:
				adj = ""
			case pc < entry:
				adj = fmt.Sprintf("-%d", entry.Sub(pc))
			default:
				adj = fmt.Sprintf("+%d", pc.Sub(entry))
			}
			fmt.Printf("  %016x %016x %s%s\n", f.Min(), f.Max(), f.Func().Name(), adj)
		}
	}
}

type typeHistogram struct {
	buckets []*bucket
	m       map[string]*bucket
	c       *gocore.Process
}

func (h *typeHistogram) add(x gocore.Object, size int64) {
	name := gocore.TypeName(h.c, x)
	b := h.m[name]
	if b == nil {
		b = &bucket{name: name, size: size, i: len(h.buckets)}
		h.buckets = append(h.buckets, b)
		h.m[name] = b
	}
	b.count++
}

func (h *typeHistogram) sort() {
	sort.Slice(h.buckets, func(i, j int) bool {
		return h.buckets[i].size*h.buckets[i].count > h.buckets[j].size*h.buckets[j].count
	})
}

func (h *typeHistogram) report(topN int, w io.Writer) {
	// report only top N if requested
	var totalSize int64
	for i := range h.buckets {
		totalSize += h.buckets[i].size * h.buckets[i].count
	}
	if topN > 0 && len(h.buckets) > topN {
		h.buckets = h.buckets[:topN]
	}
	t := tabwriter.NewWriter(w, 0, 0, 1, ' ', tabwriter.AlignRight)
	fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t %s\n", "count", "size", "bytes", "live%", "sum%", "type")
	var totalPct float64
	for _, e := range h.buckets {
		size := e.count * e.size
		pct := float64(size) / float64(totalSize) * 100
		totalPct += pct
		fmt.Fprintf(t, "%d\t%s\t%s\t%.2f\t%.2f\t %s\n", e.count,
			humanize.Bytes(uint64(e.size)),
			humanize.Bytes(uint64(size)),
			pct,
			totalPct,
			e.name)
	}
	fmt.Fprintf(t, "Total: %s\n", humanize.Bytes(uint64(totalSize)))
	t.Flush()
}

type bucket struct {
	name  string
	size  int64
	count int64
	i     int
}

func runHistogram(cmd *cobra.Command, args []string) {
	topN, err := cmd.Flags().GetInt("top")
	if err != nil {
		exitf("%v\n", err)
	}
	retained, err := cmd.Flags().GetBool("retained")
	if err != nil {
		exitf("%v\n", err)
	}
	_, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}

	// Produce an object histogram (bytes per type).
	h := typeHistogram{
		c: c,
		m: make(map[string]*bucket),
	}
	var i int
	c.ForEachObject(func(x gocore.Object) bool {
		var size int64
		if retained {
			size = c.RetainedSize(i)
		} else {
			size = c.Size(x)
		}
		h.add(x, size)
		i++
		return true
	})
	h.sort()
	h.report(topN, os.Stdout)
}

func runBreakdown(cmd *cobra.Command, args []string) {
	_, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}
	t := tabwriter.NewWriter(os.Stdout, 0, 8, 1, ' ', tabwriter.AlignRight)
	all := c.Stats().Size
	var printStat func(*gocore.Stats, string)
	printStat = func(s *gocore.Stats, indent string) {
		comment := ""
		switch s.Name {
		case "bss":
			comment = "(grab bag, includes OS thread stacks, ...)"
		case "manual spans":
			comment = "(Go stacks)"
		case "retained":
			comment = "(kept for reuse by Go)"
		case "released":
			comment = "(given back to the OS)"
		}
		fmt.Fprintf(t, "%s\t%s\t%6.2f%%\t %s\n", fmt.Sprintf("%-20s", indent+s.Name), humanize.Bytes(uint64(s.Size)),
			float64(s.Size)*100/float64(all), comment)
		for _, c := range s.Children {
			printStat(c, indent+"  ")
		}
	}
	printStat(c.Stats(), "")
	t.Flush()

}

func runObjgraph(cmd *cobra.Command, args []string) {
	_, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}

	fname := args[0]

	// Dump object graph to output file.
	w, err := os.Create(fname)
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(w, "digraph {\n")
	for k, r := range c.Globals() {
		printed := false
		c.ForEachRootPtr(r, func(i int64, y gocore.Object, j int64) bool {
			if !printed {
				fmt.Fprintf(w, "r%d [label=\"%s\n%s\",shape=hexagon]\n", k, r.Name, r.Type)
				printed = true
			}
			fmt.Fprintf(w, "r%d -> o%x [label=\"%s\"", k, c.Addr(y), typeFieldName(r.Type, i))
			if j != 0 {
				fmt.Fprintf(w, " ,headlabel=\"+%d\"", j)
			}
			fmt.Fprintf(w, "]\n")
			return true
		})
	}
	for _, g := range c.Goroutines() {
		last := fmt.Sprintf("o%x", g.Addr())
		for _, f := range g.Frames() {
			frame := fmt.Sprintf("f%x", f.Max())
			fmt.Fprintf(w, "%s [label=\"%s\",shape=rectangle]\n", frame, f.Func().Name())
			fmt.Fprintf(w, "%s -> %s [style=dotted]\n", last, frame)
			last = frame
			for _, r := range f.Roots() {
				c.ForEachRootPtr(r, func(i int64, y gocore.Object, j int64) bool {
					fmt.Fprintf(w, "%s -> o%x [label=\"%s%s\"", frame, c.Addr(y), r.Name, typeFieldName(r.Type, i))
					if j != 0 {
						fmt.Fprintf(w, " ,headlabel=\"+%d\"", j)
					}
					fmt.Fprintf(w, "]\n")
					return true
				})
			}
		}
	}
	c.ForEachObject(func(x gocore.Object) bool {
		addr := c.Addr(x)
		size := c.Size(x)
		fmt.Fprintf(w, "o%x [label=\"%s\\n%d\"]\n", addr, gocore.TypeName(c, x), size)
		c.ForEachPtr(x, func(i int64, y gocore.Object, j int64) bool {
			fmt.Fprintf(w, "o%x -> o%x [label=\"%s\"", addr, c.Addr(y), fieldName(c, x, i))
			if j != 0 {
				fmt.Fprintf(w, ",headlabel=\"+%d\"", j)
			}
			fmt.Fprintf(w, "]\n")
			return true
		})
		return true
	})
	fmt.Fprintf(w, "}")
	w.Close()
	fmt.Fprintf(os.Stderr, "wrote the object graph to %q\n", fname)
}

func runTypegraph(cmd *cobra.Command, args []string) {
	_, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}

	fname := args[0]

	// Dump object graph to output file.
	w, err := os.Create(fname)
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(w, "digraph {\n")

	type edge struct {
		count int
	}
	type blerp struct {
		e map[string]*edge
	}
	h := typeHistogram{c: c, m: make(map[string]*bucket)}
	c.ForEachObject(func(x gocore.Object) bool {
		h.add(x, c.Size(x))
		return true
	})
	m := make(map[string]*blerp)
	c.ForEachObject(func(x gocore.Object) bool {
		name := gocore.TypeName(c, x)
		b := m[name]
		if b == nil {
			b = &blerp{e: make(map[string]*edge)}
			m[name] = b
		}
		c.ForEachReversePtr(x, func(y gocore.Object, r *gocore.Root, _, _ int64) bool {
			if r == nil {
				name = gocore.TypeName(c, y)
				if b.e[name] == nil {
					b.e[name] = &edge{}
				}
				b.e[name].count++
			}
			return true
		})
		return true
	})

	for i, b := range h.buckets {
		fmt.Fprintf(w, "t%d [label=\"%s\n%d (%s)\"]\n", i, b.name, b.count, humanize.Bytes(uint64(b.size)))
		for otherName, e := range m[b.name].e {
			fmt.Fprintf(w, "t%d -> t%d [label=\"%d\"]\n", h.m[otherName].i, i, e.count)
		}
	}
	fmt.Fprintf(w, "}")
	w.Close()
	fmt.Fprintf(os.Stderr, "wrote the object graph to %q\n", fname)
}

func runPprof(cmd *cobra.Command, args []string) {
	_, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}
	fname := args[0]

	type edge struct {
		count int
	}
	type blerp struct {
		e map[string]*edge
	}
	h := typeHistogram{c: c, m: make(map[string]*bucket)}
	c.ForEachObject(func(x gocore.Object) bool {
		h.add(x, c.Size(x))
		return true
	})
	//m := make(map[string]*blerp)

	p := profile.Profile{
		SampleType: []*profile.ValueType{
			{Type: "inuse_space", Unit: "bytes"},
			{Type: "inuse_objects", Unit: "count"},
		},
		DefaultSampleType: "inuse_space",

		Location: make([]*profile.Location, len(h.buckets)),
		Sample:   make([]*profile.Sample, 0, len(h.buckets)),
		Function: make([]*profile.Function, len(h.buckets)),
	}
	typeToIdx := make(map[string]int)
	for i, b := range h.buckets {
		typeToIdx[b.name] = i
		p.Function[i] = &profile.Function{
			ID:         uint64(i + 1),
			Name:       b.name,
			SystemName: b.name,
			Filename:   "derp",
		}
		p.Location[i] = &profile.Location{
			ID: uint64(i + 1),
			Line: []profile.Line{
				{
					Line:     1,
					Function: p.Function[i],
				},
			},
		}
	}

	var f func(path []*profile.Location, x gocore.Object)
	f = func(path []*profile.Location, x gocore.Object) {
		name := gocore.TypeName(c, x)
		idx, ok := typeToIdx[name]
		if !ok {
			panic("hm")
		}
		sz := c.Size(x)
		path2 := append(path, p.Location[idx])
		location := append([]*profile.Location{}, path2...)
		for i, j := 0, len(location)-1; i < j; i, j = i+1, j-1 {
			location[i], location[j] = location[j], location[i]
		}
		p.Sample = append(p.Sample, &profile.Sample{
			Location: location,
			Value:    []int64{sz, 1},
		})
		if c.Pmark(core.Address(x)) {
			p.Sample[len(p.Sample)-1].Value[0] = 100
			p.Sample[len(p.Sample)-1].Value[1] = 1
			return
		}
		c.ForEachPtr(x, func(_ int64, y gocore.Object, _ int64) bool {
			f(path2, y)
			return true
		})
	}

	skippedNames := []string{"runtime", "unicode", "reflect.", "io", "sync.", "syscall.", "strconv.", "time.",
		"internal/", "os.", "_cgo", "fmt", "math.", "path.", "errors."}
	c.ForEachRoot(func(r *gocore.Root) bool {
		for _, n := range skippedNames {
			if strings.HasPrefix(r.Name, n) {
				return true
			}
		}
		t := r.Type.String()
		idx, ok := typeToIdx[t]
		if !ok {
			idx = len(p.Location)
			typeToIdx[t] = idx
			p.Function = append(p.Function, &profile.Function{
				ID:         uint64(idx + 1),
				Name:       t,
				SystemName: t,
				Filename:   "derp",
			})
			p.Location = append(p.Location, &profile.Location{
				ID: uint64(idx + 1),
				Line: []profile.Line{
					{
						Line:     1,
						Function: p.Function[idx],
					},
				},
			})
		}
		// For every root, we emit a 1-"frame" sample to the profile so that
		// we can make sure to see the size of all of the roots as well as
		// their children.
		path := []*profile.Location{p.Location[idx]}
		p.Sample = append(p.Sample, &profile.Sample{
			Location: path,
			Value:    []int64{r.Type.Size, 1},
		})
		c.ForEachRootPtr(r, func(a int64, x gocore.Object, b int64) bool {
			f(path, x)
			return true
		})
		return true
	})

	fmt.Println(p.CheckValid())
	w, err := os.Create(fname)
	if err != nil {
		panic(err)
	}
	p.Write(w)
	w.Close()
}

func runObjects(cmd *cobra.Command, args []string) {
	_, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}
	c.ForEachObject(func(x gocore.Object) bool {
		fmt.Printf("%16x %s\n", c.Addr(x), gocore.TypeName(c, x))
		return true
	})

}

func runReachable(cmd *cobra.Command, args []string) {
	_, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}
	n, err := strconv.ParseInt(args[0], 16, 64)
	if err != nil {
		fmt.Printf("can't parse %q as an object address\n", args[0])
		return
	}
	a := core.Address(n)
	obj, _ := c.FindObject(a)
	if obj == 0 {
		fmt.Printf("can't find object at address %s\n", args[0])
		return
	}

	// Breadth-first search backwards until we reach a root.
	type hop struct {
		i int64         // offset in "from" object (the key in the path map) where the pointer is
		x gocore.Object // the "to" object
		j int64         // the offset in the "to" object
	}
	depth := map[gocore.Object]int{}
	depth[obj] = 0
	q := []gocore.Object{obj}
	done := false
	for !done {
		if len(q) == 0 {
			panic("can't find a root that can reach the object")
		}
		y := q[0]
		q = q[1:]
		c.ForEachReversePtr(y, func(x gocore.Object, r *gocore.Root, i, j int64) bool {
			if r != nil {
				// found it.
				if r.Frame == nil {
					// Print global
					fmt.Printf("%s", r.Name)
				} else {
					// Print stack up to frame in question.
					var frames []*gocore.Frame
					for f := r.Frame.Parent(); f != nil; f = f.Parent() {
						frames = append(frames, f)
					}
					for k := len(frames) - 1; k >= 0; k-- {
						fmt.Printf("%s\n", frames[k].Func().Name())
					}
					// Print frame + variable in frame.
					fmt.Printf("%s.%s", r.Frame.Func().Name(), r.Name)
				}
				fmt.Printf("%s → \n", typeFieldName(r.Type, i))

				z := y
				for {
					fmt.Printf("%x %s", c.Addr(z), gocore.TypeName(c, z))
					if z == obj {
						fmt.Println()
						break
					}
					// Find an edge out of z which goes to an object
					// closer to obj.
					c.ForEachPtr(z, func(i int64, w gocore.Object, j int64) bool {
						if d, ok := depth[w]; ok && d < depth[z] {
							fmt.Printf(" %s → %s", objField(c, z, i), objRegion(c, w, j))
							z = w
							return false
						}
						return true
					})
					fmt.Println()
				}
				done = true
				return false
			}
			if _, ok := depth[x]; ok {
				// we already found a shorter path to this object.
				return true
			}
			depth[x] = depth[y] + 1
			q = append(q, x)
			return true
		})
	}
}

// httpServer is the singleton http server, initialized by
// the first call to runHTML.
var httpServer struct {
	sync.Mutex
	port int
}

func runHTML(cmd *cobra.Command, args []string) {
	httpServer.Lock()
	defer httpServer.Unlock()
	if httpServer.port != 0 {
		fmt.Printf("already serving on http://localhost:%d\n", httpServer.port)
		return
	}
	_, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}

	port, err := cmd.Flags().GetInt("port")
	if err != nil {
		exitf("%v\n", err)
	}
	serveHTML(c, port, cfg.interactive)
	httpServer.port = port
	// TODO: launch web browser
}

func runRead(cmd *cobra.Command, args []string) {
	p, _, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}
	n, err := strconv.ParseInt(args[0], 16, 64)
	if err != nil {
		exitf("can't parse %q as an object address\n", args[1])
	}
	a := core.Address(n)
	if len(args) < 2 {
		n = 256
	} else {
		n, err = strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			exitf("can't parse %q as a byte count\n", args[2])
		}
	}
	if !p.ReadableN(a, n) {
		exitf("address range [%x,%x] not readable\n", a, a.Add(n))
	}
	b := make([]byte, n)
	p.ReadAt(b, a)
	for i, x := range b {
		if i%16 == 0 {
			if i > 0 {
				fmt.Println()
			}
			fmt.Printf("%x:", a.Add(int64(i)))
		}
		fmt.Printf(" %02x", x)
	}
	fmt.Println()
}

func runPeek(cmd *cobra.Command, args []string) {
	_, c, err := readCore()
	if err != nil {
		exitf("%v\n", err)
	}
	typName := args[0]

	inH := typeHistogram{
		c: c,
		m: make(map[string]*bucket),
	}
	h := typeHistogram{
		c: c,
		m: make(map[string]*bucket),
	}
	outH := typeHistogram{
		c: c,
		m: make(map[string]*bucket),
	}
	c.ForEachObject(func(x gocore.Object) bool {
		typ := gocore.TypeName(c, x)
		if typ == typName {
			size := c.Size(x)
			h.add(x, size)
			c.ForEachReversePtr(x, func(y gocore.Object, r *gocore.Root, i, j int64) bool {
				if r == nil {
					inH.add(y, size)
				}
				return true
			})
			c.ForEachPtr(x, func(i int64, y gocore.Object, j int64) bool {
				outH.add(y, c.Size(y))
				return true
			})
		}
		return true
	})
	inH.sort()
	inH.report(0, os.Stdout)
	h.sort()
	h.report(0, os.Stdout)
	outH.sort()
	outH.report(0, os.Stdout)

}

// fieldName returns the name of the field at offset off in x.
func fieldName(c *gocore.Process, x gocore.Object, off int64) string {
	size := c.Size(x)
	typ, repeat := c.Type(x)
	if typ == nil {
		return fmt.Sprintf("f%d", off)
	}
	n := size / typ.Size
	i := off / typ.Size
	if i == 0 && repeat == 1 {
		// Probably a singleton object, no need for array notation.
		return typeFieldName(typ, off)
	}
	if i >= n {
		// Partial space at the end of the object - the type can't be complete.
		return fmt.Sprintf("f%d", off)
	}
	q := ""
	if i >= repeat {
		// Past the known repeat section, add a ? because we're not sure about the type.
		q = "?"
	}
	return fmt.Sprintf("[%d]%s%s", i, typeFieldName(typ, off-i*typ.Size), q)
}

// typeFieldName returns the name of the field at offset off in t.
func typeFieldName(t *gocore.Type, off int64) string {
	switch t.Kind {
	case gocore.KindBool, gocore.KindInt, gocore.KindUint, gocore.KindFloat:
		return ""
	case gocore.KindComplex:
		if off == 0 {
			return ".real"
		}
		return ".imag"
	case gocore.KindIface, gocore.KindEface:
		if off == 0 {
			return ".type"
		}
		return ".data"
	case gocore.KindPtr, gocore.KindFunc:
		return ""
	case gocore.KindString:
		if off == 0 {
			return ".ptr"
		}
		return ".len"
	case gocore.KindSlice:
		if off == 0 {
			return ".ptr"
		}
		if off <= t.Size/2 {
			return ".len"
		}
		return ".cap"
	case gocore.KindArray:
		s := t.Elem.Size
		i := off / s
		return fmt.Sprintf("[%d]%s", i, typeFieldName(t.Elem, off-i*s))
	case gocore.KindStruct:
		for _, f := range t.Fields {
			if f.Off <= off && off < f.Off+f.Type.Size {
				return "." + f.Name + typeFieldName(f.Type, off-f.Off)
			}
		}
	}
	return ".???"
}

func startProfile() {
	if cfg.cpuprof != "" {
		f, err := os.Create(cfg.cpuprof)
		if err != nil {
			fmt.Fprintf(os.Stderr, "can't open profile file: %s\n", err)
			os.Exit(2)
		}
		pprof.StartCPUProfile(f)

	}
}

func endProfile() {
	if cfg.cpuprof != "" {
		pprof.StopCPUProfile()
	}
}

func exitf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format, args...)
	os.Exit(1)
}
