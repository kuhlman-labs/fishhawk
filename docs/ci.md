# CI and deploy workflows

Reference notes for the GitHub Actions surfaces. The workflow files
under `.github/workflows/` are **human-led** (agent runs must not edit
them); this doc carries the operational contract.

## Backend deploy workflow (E13.7.4 / [#168](https://github.com/kuhlman-labs/fishhawk/issues/168))

`.github/workflows/backend-deploy.yml` closes the deploy chain for the
Terraform-managed ECS stack (`infra/terraform/README.md`):

- **Trigger**: `backend-release.yml` completion, or `workflow_dispatch`
  for rollback (dispatch manually with an explicit older `image_tag`).
- **Auth**: assumes the `<project>-<env>-gha-deploy` OIDC role; the
  role's trust policy is scoped to `main` + `backend/v*` tags.
- **Steps**: registers new task-definition revisions for BOTH the serve
  and migrate families with the new image; runs the migration task to
  completion (failures surface CloudWatch logs); then
  `aws-actions/amazon-ecs-deploy-task-definition` waits for the ECS
  service to converge.
- **Rollback**: the service's deployment circuit breaker
  auto-rolls-back on health-check failures; the workflow surfaces the
  rollback as a workflow error.
- **Smoke test**: hits `/healthz` against the ALB after stability.
- **One-time setup**: the operator sets the `AWS_DEPLOY_ROLE_ARN` repo
  variable (output from Terraform) once per environment.
