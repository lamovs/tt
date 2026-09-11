package cli

import (
	"errors"
	"fmt"

	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
)

func ResolveDefaultProject(projects []model.Project, value string) (model.Project, error) {
	id, name, err := config.ProjectReference(value)
	if err != nil {
		return model.Project{}, err
	}
	var matches []model.Project
	if id != "" {
		for _, p := range projects {
			if p.Id == id {
				matches = append(matches, p)
			}
		}
	} else {
		_, names := MatchListName(projectNames(projects), name)
		for _, p := range projects {
			for _, matched := range names {
				if p.Name == matched {
					matches = append(matches, p)
					break
				}
			}
		}
	}
	if len(projects) == 0 {
		return model.Project{}, errors.New("no cached lists; run tt sync, then tt config default-project")
	}
	switch len(matches) {
	case 0:
		return model.Project{}, errors.New("default_project matches no cached list; the cache may be stale or the list renamed or removed; sync and choose again")
	case 1:
		if reason := matches[0].CreateUnavailable(); reason != "" {
			return model.Project{}, fmt.Errorf("default_project is unusable in the cache: %s; sync or choose another list", reason)
		}
		return matches[0], nil
	default:
		return model.Project{}, fmt.Errorf("default_project matches %d cached lists; choose an ID with tt config default-project", len(matches))
	}
}
