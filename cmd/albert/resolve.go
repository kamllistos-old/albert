package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/kamllistos/albert/internal/provider"
)

// resolveSource decides which source an installed tool belongs to. With
// source set it checks the tool is there. Otherwise one match is used as is;
// several are put to the user as a numbered menu when tty is true, and are an
// error when it is not.
func resolveSource(tools []provider.Tool, name, source string, in io.Reader, out io.Writer, tty bool) (string, error) {
	var sources []string
	for _, t := range tools {
		if t.Name != name || contains(sources, t.Source) {
			continue
		}
		sources = append(sources, t.Source)
	}
	if source != "" {
		if !contains(sources, source) {
			return "", fmt.Errorf("%q isn't installed from %s.", name, source)
		}
		return source, nil
	}
	switch len(sources) {
	case 0:
		return "", fmt.Errorf("Couldn't find %q in your installed tools. Try \"albert list\".", name)
	case 1:
		return sources[0], nil
	}
	return chooseSource(name, "is installed from", sources, in, out, tty)
}

// chooseSource asks the user to pick one of several sources.
func chooseSource(name, verb string, sources []string, in io.Reader, out io.Writer, tty bool) (string, error) {
	if !tty {
		return "", fmt.Errorf("%q %s several sources (%s). Add --source.", name, verb, strings.Join(sources, ", "))
	}
	fmt.Fprintf(out, "%q %s several sources:\n", name, verb)
	for i, s := range sources {
		fmt.Fprintf(out, "  %d) %s", i+1, s)
	}
	fmt.Fprintf(out, "\nWhich one? [1-%d]: ", len(sources))
	if in == nil {
		return "", errors.New("No answer was given.")
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", errors.New("No answer was given.")
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(sources) {
		return "", fmt.Errorf("%q isn't one of the choices.", strings.TrimSpace(line))
	}
	return sources[n-1], nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
