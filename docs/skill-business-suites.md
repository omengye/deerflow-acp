# Business behavior suites

The package includes `skill-behavior/code-documentation.json` and
`skill-behavior/newsletter-generation.json` under resources (ACP packages put
them at the top level). Each contains one targeted and one regression case with
one repetition, supplied local facts and declarative assertions.

These fixtures are available to SDK integrations that explicitly configure a
behavior evaluator. Both use the Go registry's `python-public` source ID.
The native desktop publishes skill creations and updates automatically after
security review of the complete package.
