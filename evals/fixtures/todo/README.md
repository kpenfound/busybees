# todo

A to-do list kept in a plain text file, one item per line:

```
x (A) file the tax return due:2026-04-15
(B) buy milk
call the plumber due:2026-03-01
```

- `x ` at the start marks a done item.
- `(A)` to `(Z)` is the priority, `(A)` the highest. An item may have none.
- `due:YYYY-MM-DD` anywhere in the line is the due date.

The `todo` package reads and writes that format. The `todo` command works on
`todo.txt` in the current directory, or on the file `-f` names:

```sh
go run ./cmd/todo add "buy milk"
go run ./cmd/todo list
go run ./cmd/todo done 1
```

Run the tests with `go test ./...`.
