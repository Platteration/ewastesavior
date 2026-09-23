package config

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// LoadDefault loads the merged config the way services do: the merged file
// written at boot when present, else the default file list plus /proc/cmdline.
func LoadDefault(explicit string) (Config, []string, error) {
	if explicit != "" {
		return Load([]string{explicit}, "")
	}
	if _, err := os.Stat(MergedConfigFile); err == nil {
		return Load([]string{MergedConfigFile}, "")
	}
	cmdline, _ := os.ReadFile(CmdlineFile)
	return Load(DefaultFiles, string(cmdline))
}

// Main implements `savior config`.
func Main(args []string) int {
	return run(args, os.Stdout, os.Stderr)
}

func run(args []string, stdout, stderr io.Writer) int {
	usage := func() {
		fmt.Fprintln(stderr, `usage: savior config <command> [flags]

commands:
  env     print the merged config as shell assignments (SAVIOR_KEY='value')
  get     print the value of one or more keys
  dump    write the merged config file (--out PATH, default stdout)
  keys    list all keys with defaults
  sample  print a commented savior.conf template

flags (env, get, dump):
  --file PATH          config file to read (repeatable; default /etc/savior/savior.conf, /media/savior/savior.conf)
  --cmdline-file PATH  kernel command line to read (default /proc/cmdline; "" = none)
  --quiet              don't print warnings`)
	}
	if len(args) == 0 {
		usage()
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "keys":
		tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "KEY\tTYPE\tDEFAULT\tDESCRIPTION")
		for _, k := range Keys() {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", k.Name, k.Type, k.Default, k.Help)
		}
		tw.Flush()
		return 0
	case "sample":
		writeSample(stdout)
		return 0
	case "env", "get", "dump":
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(stderr, "savior config: unknown command %q\n", cmd)
		usage()
		return 2
	}

	fs := flag.NewFlagSet("config "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var files stringList
	fs.Var(&files, "file", "config file (repeatable)")
	cmdlineFile := fs.String("cmdline-file", CmdlineFile, "kernel command line file")
	out := fs.String("out", "", "output file (dump)")
	quiet := fs.Bool("quiet", false, "suppress warnings")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if len(files) == 0 {
		files = DefaultFiles
	}
	cmdline := ""
	if *cmdlineFile != "" {
		if b, err := os.ReadFile(*cmdlineFile); err == nil {
			cmdline = string(b)
		}
	}
	c, warnings, err := Load(files, cmdline)
	if err != nil {
		fmt.Fprintf(stderr, "savior config: %v\n", err)
		return 1
	}
	if !*quiet {
		for _, w := range warnings {
			fmt.Fprintf(stderr, "savior config: warning: %s\n", w)
		}
	}
	switch cmd {
	case "env":
		fmt.Fprint(stdout, c.ShellEnv())
	case "get":
		if fs.NArg() == 0 {
			fmt.Fprintln(stderr, "savior config get: need at least one key")
			return 2
		}
		rc := 0
		for _, k := range fs.Args() {
			vals, ok := c.Values(k)
			if !ok {
				fmt.Fprintf(stderr, "savior config get: unknown key %q\n", k)
				rc = 1
				continue
			}
			for _, v := range vals {
				fmt.Fprintln(stdout, v)
			}
		}
		return rc
	case "dump":
		if *out == "" {
			if err := c.WriteFile(stdout); err != nil {
				fmt.Fprintf(stderr, "savior config dump: %v\n", err)
				return 1
			}
			return 0
		}
		if err := writeFileAtomic(*out, c); err != nil {
			fmt.Fprintf(stderr, "savior config dump: %v\n", err)
			return 1
		}
	}
	return 0
}

func writeFileAtomic(path string, c Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".savior.conf.*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := c.WriteFile(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeSample(w io.Writer) {
	fmt.Fprint(w, `# savior.conf - SaviorOS node configuration
#
# Put this file in the root of the SAVIOR partition of your boot stick.
# You can edit it on any computer (Windows, macOS, Linux).
# Format: key = value. Lines starting with # are comments.
# Any key can also be set on the kernel command line as savior.<key>=<value>.
#
# The only setting most swarms need is swarm_key. Use the same key on every
# machine and on the hive. Generate one with: savior ctl genkey
`)
	for _, k := range Keys() {
		fmt.Fprintf(w, "\n# %s\n", k.Help)
		if k.Default != "" {
			fmt.Fprintf(w, "# default: %s\n", k.Default)
		}
		fmt.Fprintf(w, "#%s = %s\n", k.Name, k.Default)
	}
}
