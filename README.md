# cost-notify

AWS Lambda functions for cost and billing notifications, deployed via [tamura09/aws-terraform](https://github.com/tamura09/aws-terraform).

## Functions

- `aws-monthly-billing` — Monthly AWS resource usage and billing notification for Discord.
- `aws-weekly-billing` — Weekly AWS resource usage and billing notification for Discord.
- `hetzner-billing` — Monthly Hetzner Cloud billing estimate notification for Discord.

Each function is an independent Go module built for `provided.al2023` / `arm64`.

## CI/CD

[.github/workflows/build.yml](.github/workflows/build.yml) tests every function on pull requests and pushes to `main`. On push to `main`, it also builds each function's `bootstrap` binary, zips it, and uploads the artifact to the Lambda artifact S3 bucket that `tamura09/aws-terraform` reads from (`s3://aws-terraform-lambda-artifacts-<account_id>-us-east-1/lambda/<function>.zip`).

Uploads authenticate via GitHub OIDC, assuming the `github-actions-lambda-artifacts` IAM role defined in `tamura09/aws-terraform`.

Terraform in `tamura09/aws-terraform` still owns the AWS resources (Lambda function, IAM role, EventBridge schedule, CloudWatch log group); this repository only owns the function source and its build/upload pipeline.
