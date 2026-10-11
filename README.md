# cost-notify

AWS Lambda functions for cost and billing notifications, deployed via [tamura09/terraform](https://github.com/tamura09/terraform).

## Functions

- `aws-monthly-billing` — Monthly AWS resource usage and billing notification for Discord.
- `aws-weekly-billing` — Weekly AWS resource usage and billing notification for Discord.
- `github-billing` — Monthly GitHub metered usage and billing notification for Discord. Runs on the 1st and reports the previous UTC month from the GitHub billing usage report.
- `hetzner-billing` — Monthly Hetzner Cloud billing estimate notification for Discord.

Each function is an independent Go module built for `provided.al2023` / `arm64`.

## CI/CD

[.github/workflows/build.yml](.github/workflows/build.yml) tests every function on pull requests only; the `Test` checks are required, so nothing reaches `main` without passing them. On push to `main` (and on `workflow_dispatch` from `main`), it builds each function's `bootstrap` binary, zips it, uploads the artifact to the Lambda artifact S3 bucket that `tamura09/terraform` reads from (`s3://aws-terraform-lambda-artifacts-<account_id>-us-east-1/lambda/<function>.zip`), and immediately calls `aws lambda update-function-code` so the deployed Lambda runs the new code right away.

A function that does not exist yet is skipped with a notice instead of failing the job: Terraform creates a function from the artifact at its fixed key, so a newly added function's first deploy uploads before the function can exist. Re-run the workflow after the Terraform apply that creates it. Any other error from the existence check fails the job.

AWS calls authenticate via GitHub OIDC, assuming the `github-actions-lambda-deploy-cost-notify` IAM role defined in `tamura09/terraform` (scoped to S3 upload, `lambda:GetFunctionConfiguration` and `lambda:UpdateFunctionCode` on these specific functions). A new function has to be added to that role before its first deploy.

Terraform in `tamura09/terraform` owns the AWS resources (Lambda function, IAM role, EventBridge schedule, CloudWatch log group, SSM parameters) but does not track the artifact version, so code reaches the functions only through this workflow; this repository owns the function source and its build/deploy pipeline.
