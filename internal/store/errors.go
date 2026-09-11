package store

import "errors"

var (
	ErrNotFound = errors.New("not in the local cache")

	ErrNoListing = errors.New("no listing to resolve numbers against")

	ErrNoUndo = errors.New("nothing to undo")
)
