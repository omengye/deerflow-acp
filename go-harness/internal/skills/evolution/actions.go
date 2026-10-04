package evolution

import (
	"context"
	"path"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func validAction(action string) bool {
	switch action {
	case "create", "edit", "patch", "delete", "write_file", "remove_file":
		return true
	}
	return false
}

// Materialization always uses the caller's immutable base, even after that
// base has been superseded. The later submit transaction checks idempotency
// first, then CAS, allowing a completed request to be safely retried.
func (s *Service) materialize(ctx context.Context, r Request) (Request, error) {
	if !validAction(r.Action) {
		return r, invalid("unknown candidate action")
	}
	complete := r.Files != nil
	if complete {
		if r.Content != nil || r.Find != nil || r.Replace != nil || r.ExpectedCount != nil || r.Path != "" || (r.Action != "create" && r.Action != "edit") {
			return r, invalid("complete files cannot be mixed with mutation arguments")
		}
		return r, nil
	}
	if r.Action == "delete" {
		if r.Content != nil || r.Find != nil || r.Replace != nil || r.ExpectedCount != nil || r.Path != "" {
			return r, invalid("delete does not accept mutation arguments")
		}
		return r, nil
	}
	switch r.Action {
	case "create", "edit":
		if r.Content == nil || r.Find != nil || r.Replace != nil || r.ExpectedCount != nil || r.Path != "" {
			return r, invalid("create/edit requires only content")
		}
	case "patch":
		if r.Content != nil || r.Find == nil || r.Replace == nil || *r.Find == "" || r.Path != "" || (r.ExpectedCount != nil && (*r.ExpectedCount < 1 || *r.ExpectedCount > 10000)) {
			return r, invalid("patch requires nonempty find and replacement")
		}
	case "write_file", "remove_file":
		if r.Find != nil || r.Replace != nil || r.ExpectedCount != nil || r.Path == "" || len(r.Path) > 512 || strings.ContainsAny(r.Path, "\\:\x00") || strings.HasPrefix(r.Path, "/") || path.Clean(r.Path) != r.Path || strings.HasPrefix(r.Path, "../") || r.Path == ".." || strings.EqualFold(r.Path, "SKILL.md") || (r.Action == "write_file") != (r.Content != nil) {
			return r, invalid("invalid support-file mutation")
		}
	}
	for _, text := range []*string{r.Content, r.Find, r.Replace} {
		if text != nil && !bounded(*text, 128<<10) {
			return r, invalid("mutation text exceeds bounds")
		}
	}
	files := make(map[string][]byte)
	if r.Action != "create" {
		if !r.Base.Exists || r.Base.Version == "" {
			return r, invalid("mutation requires an installed base")
		}
		err := s.registry.WithPublication(ctx, r.SourceID, r.Name, func(tx *skills.PublicationTx) error {
			candidate, err := tx.ReadVersion(r.Base.Version)
			if err != nil {
				return err
			}
			files = candidate.Files()
			return nil
		})
		if err != nil {
			return r, err
		}
	}
	switch r.Action {
	case "create", "edit":
		files["SKILL.md"] = []byte(*r.Content)
	case "patch":
		before := string(files["SKILL.md"])
		found := strings.Count(before, *r.Find)
		count := 1
		if r.ExpectedCount != nil {
			count = *r.ExpectedCount
			if found != count {
				return r, invalid("patch occurrence count differs from expected_count")
			}
		}
		if found == 0 {
			return r, invalid("patch target was not found")
		}
		files["SKILL.md"] = []byte(strings.Replace(before, *r.Find, *r.Replace, count))
	case "write_file":
		files[r.Path] = []byte(*r.Content)
	case "remove_file":
		if _, exists := files[r.Path]; !exists {
			return r, invalid("support file does not exist")
		}
		delete(files, r.Path)
	}
	r.Files = files
	r.Content, r.Find, r.Replace, r.ExpectedCount = nil, nil, nil, nil
	r.Path = ""
	return r, nil
}
