// The Go files under evals/ belong to the eval fixtures, not to busybees:
// this file keeps them out of the root module's ./... . Nothing builds this
// module; dagger.toml leaves evals/ out of its module selection too.
module github.com/kpenfound/busybees/evals

go 1.22
