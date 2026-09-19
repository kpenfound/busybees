// Command todo works on a to-do list file.
//
//	todo [-f file] list         print the items, numbered from 1
//	todo [-f file] add <title>  add an item at the end
//	todo [-f file] done <n>     mark item n done
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"example.com/todo/todo"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "todo:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("todo", flag.ContinueOnError)
	fs.SetOutput(out)
	file := fs.String("f", "todo.txt", "the to-do list file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	args = fs.Args()
	if len(args) == 0 {
		return errors.New("usage: todo [-f file] list|add|done")
	}
	l, err := load(*file)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		for i, it := range l {
			fmt.Fprintf(out, "%d %s\n", i+1, it)
		}
		return nil
	case "add":
		it, err := todo.Parse(strings.Join(args[1:], " "))
		if err != nil {
			return err
		}
		return save(*file, append(l, it))
	case "done":
		if len(args) != 2 {
			return errors.New("usage: todo done <n>")
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("item number %q: %w", args[1], err)
		}
		if err := l.Complete(n); err != nil {
			return err
		}
		return save(*file, l)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

// load reads the list in file; a file that does not exist is an empty list.
func load(file string) (todo.List, error) {
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return todo.Read(f)
}

func save(file string, l todo.List) error {
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	if err := l.Write(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
