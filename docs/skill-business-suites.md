# Business behavior suites

The package includes `skill-behavior/code-documentation.json` and
`skill-behavior/newsletter-generation.json` under resources (ACP packages put
them at the top level). Each contains one targeted and one regression case with
one repetition, supplied local facts and declarative assertions.

Select **one** suite via `skill_evolution.behavior.suite_path`, using a path
relative to the configuration file or an absolute path. Both target the existing
`python-public` source ID; this is a compatibility identifier used by the Go
registry. Built-in candidate evaluation requires explicitly permitting built-in
overlays in the existing evolution settings. Keep automatic publication disabled
while reviewing candidates. Passing these bounded fixtures does not certify
live model quality.
