package filesearch

import (
	"context"
	"errors"
)

// Files visits regular files selected by query in deterministic lexical path
// order. Discovered symlinks are not followed. Returning ErrStop from visit is
// a successful early stop; other callback errors are returned unchanged.
func (s *Searcher) Files(ctx context.Context, query FilesQuery, visit func(File) error) (Stats, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if visit == nil {
		return Stats{}, errors.New("filesearch: Files callback is nil")
	}
	direct, err := compileDirectPattern(query.Glob)
	if err != nil {
		return Stats{}, err
	}
	directory, err := s.resolveDirectory(query.Path)
	if err != nil {
		return Stats{}, err
	}
	walker := fileWalker{
		searcher: s,
		query:    directory,
		direct:   direct,
		visit: func(file walkedFile) error {
			return visit(File{Path: file.path})
		},
	}
	return walker.run(ctx)
}
