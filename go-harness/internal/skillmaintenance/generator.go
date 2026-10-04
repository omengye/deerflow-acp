package skillmaintenance

import "github.com/omengye/deerflow-acp/go-harness/harness"

type GenerationConfig = harness.SkillGenerationModelConfig

// GenerationInput is assembled by the host from a pinned target/base and
// bounded evidence. A generator has no authority to select a different target.
type GenerationInput = harness.SkillGenerationInput

type Evidence = harness.SkillGenerationEvidence

// Candidate is an unvalidated offer. The evolution service must apply its
// deterministic checks, independent review and publication CAS before use.
type Candidate = harness.SkillGeneratedCandidate

type Generator = harness.SkillCandidateGenerator
