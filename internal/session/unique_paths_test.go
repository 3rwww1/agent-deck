package session

import (
	"errors"
	"strings"
	"testing"
)

func uniquePathsConfig(t *testing.T, body string) {
	t.Helper()
	_, config, _, _ := isolateConfigRoots(t)
	writeConfigAt(t, xdgAgentDeckConfigDir(config), body)
	ClearUserConfigCache()
}

func uniquePathsFixture() []*Instance {
	return []*Instance{
		{ID: "a", Title: "lane", GroupPath: "team"},
		{ID: "b", Title: "lane", GroupPath: "other"},
		{ID: "c", Title: "solo", GroupPath: ""},
	}
}

func TestCheckUniquePathRefusesSameTitleInSameGroup(t *testing.T) {
	uniquePathsConfig(t, "unique_paths = true")
	newcomer := &Instance{ID: "new", Title: "lane", GroupPath: "team"}
	err := CheckUniquePath(uniquePathsFixture(), newcomer, "team", "lane")
	var taken *PathTakenError
	if !errors.As(err, &taken) {
		t.Fatalf("CheckUniquePath = %v, want PathTakenError", err)
	}
	if taken.Other.ID != "a" || !strings.Contains(err.Error(), "team/lane") || !strings.Contains(err.Error(), "a") {
		t.Fatalf("error = %q, want it to name team/lane and session a", err)
	}
}

func TestCheckUniquePathAllowsSameTitleInOtherGroup(t *testing.T) {
	uniquePathsConfig(t, "unique_paths = true")
	newcomer := &Instance{ID: "new", Title: "lane", GroupPath: "third"}
	if err := CheckUniquePath(uniquePathsFixture(), newcomer, "third", "lane"); err != nil {
		t.Fatalf("CheckUniquePath = %v, want nil for a title only used in other groups", err)
	}
}

func TestCheckUniquePathRefusesMoveOntoTakenPath(t *testing.T) {
	uniquePathsConfig(t, "unique_paths = true")
	instances := uniquePathsFixture()
	if err := CheckUniquePath(instances, instances[1], "team", "lane"); err == nil {
		t.Fatal("moving other/lane into team accepted, want refusal")
	}
}

func TestCheckUniquePathIgnoresSelfAndDefaultGroup(t *testing.T) {
	uniquePathsConfig(t, "unique_paths = true")
	instances := uniquePathsFixture()
	if err := CheckUniquePath(instances, instances[0], "team", "lane"); err != nil {
		t.Fatalf("renaming a session to its own title = %v, want nil", err)
	}
	if err := CheckUniquePath(instances, &Instance{ID: "new"}, DefaultGroupPath, "solo"); err == nil {
		t.Fatal("an empty group path and the default group must be one group")
	}
}

func TestCheckUniquePathOffByDefault(t *testing.T) {
	uniquePathsConfig(t, "")
	if err := CheckUniquePath(uniquePathsFixture(), &Instance{ID: "new"}, "team", "lane"); err != nil {
		t.Fatalf("CheckUniquePath with unique_paths unset = %v, want nil", err)
	}
}

func TestDuplicatePathsListsOnlySameGroupClashes(t *testing.T) {
	instances := append(uniquePathsFixture(), &Instance{ID: "d", Title: "lane", GroupPath: "team"})
	dups := DuplicatePaths(instances)
	if len(dups) != 1 || dups[0].Path != "team/lane" || len(dups[0].Sessions) != 2 {
		t.Fatalf("DuplicatePaths = %+v, want only team/lane held by a and d", dups)
	}
	if instances[3].Title != "lane" {
		t.Fatal("DuplicatePaths renamed a session")
	}
}
