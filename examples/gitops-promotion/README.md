# Promote a template version after its test passed

This Kustomize directory runs a `CoderTemplateTest` of one version of the
template `default.docker`, and promotes the version only after the test
passed. It works with Argo CD (`argocd-application.yaml`) and Flux
(`flux-kustomization.yaml`).

[Gate template promotion with GitOps](../../docs/how-to/gitops.md#run-the-promotion-example)
explains each file, the checks of `promote.sh`, and the CI steps for a new
version.

`promote_test.go` renders this directory and runs `promote.sh` against a fake
`kubectl`. `make test` runs it.
