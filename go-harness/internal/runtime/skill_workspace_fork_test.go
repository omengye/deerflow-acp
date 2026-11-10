package runtime

import (
	"context"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
)

func TestWorkspaceAndReadonlySkillImprovementsCreateIndependentCandidates(t *testing.T) {
	for _, mode := range []string{"foreign", "readonly", "builtin-authorized"} {
		readOnly := mode != "foreign"
		name := "foreign-workspace"
		if readOnly {
			name = mode
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s, x, r, _, _ := newMatchingTestSources(t, true, readOnly)
			var original harness.SkillHead
			if err := r.WithPublication(ctx, "project", "json-check", func(pub *skills.PublicationTx) error {
				var err error
				original, err = pub.Current()
				if err != nil || readOnly {
					return err
				}
				candidate, err := r.PrepareCandidate("json-check", map[string][]byte{"SKILL.md": []byte("---\nname: json-check\ndescription: Foreign workspace private procedure\n---\nForeign-only method\n")})
				if err != nil {
					return err
				}
				version, err := pub.Stage(candidate)
				if err != nil {
					return err
				}
				_, err = pub.PublishAutomatic(version, original, true, t.TempDir())
				return err
			}); err != nil {
				t.Fatal(err)
			}
			g, v := &experienceDiscoveryGenerator{}, &experienceDiscoveryValidator{}
			e, err := evolution.New(ctx, r, v)
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			destination := "project"
			if readOnly {
				destination = "owned"
			}
			sources := []string{destination}
			if mode == "builtin-authorized" {
				sources = append(sources, "project")
			}
			if err = e.ConfigureGeneration(ctx, g, consolidationTestOptions().Policy, sources, mode == "builtin-authorized"); err != nil {
				t.Fatal(err)
			}
			if err = e.ConfigureExperienceDiscoverySource(s.Store.Learning); err != nil {
				t.Fatal(err)
			}
			if err = e.ConfigureDiscovery(harness.SkillDiscoveryOptions{DestinationSourceID: destination, RepeatThreshold: 2, RepeatWindowDays: 30, CooldownHours: 24, MaxPendingProposals: 20}); err != nil {
				t.Fatal(err)
			}
			experienceTwoEpisodes(t, s, x)
			ds := experienceDiscoveries(t, e)
			if len(ds) != 1 || ds[0].Target.SourceID != destination || ds[0].Target.Name != skills.WorkspaceSkillName("json-check-extension", x.CWD) || ds[0].Base.Exists {
				t.Fatal("workspace improvement permanently conflicted", ds)
			}
			p := experienceGenerate(t, e)
			if p.Action != "create" || p.Status != "pending_review" || len(g.seen) != 1 || len(g.seen[0].BaseFiles) != 0 {
				t.Fatal("fork did not retain automatic candidate pipeline", p.Status)
			}
			if _, err := e.Approve(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"}); err == nil {
				t.Fatal("fork bypassed automatic validation")
			}
			if err := r.WithPublication(ctx, "project", "json-check", func(pub *skills.PublicationTx) error {
				pkg, err := pub.ReadVersion(original.Version)
				if err == nil && len(pkg.Files()["SKILL.md"]) == 0 {
					t.Fatal("original baseline lost")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
