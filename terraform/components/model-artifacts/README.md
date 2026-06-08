# components/model-artifacts

Two CMK-encrypted S3 buckets used by tenants for storing:

- **Model artifacts** — LoRA / adapter / fine-tuned weights. Layout enforced by operator: `tenants/<platform-id>/<artifact-kind>/<artifact-id>/...`.
- **Eval reports** — eval suite outputs (HTML + JSON + JUnit XML). Read by Grafana dashboards via Athena.

Encryption uses `cmk-data` from `landing-zone`. Bucket policy denies non-TLS access and non-KMS uploads.

## Inputs

| Variable                               | Description                                           |
| -------------------------------------- | ----------------------------------------------------- |
| `environment`                          | dev / staging / production                            |
| `region`                               | AWS region                                            |
| `cluster_name`                         | EKS cluster name                                      |
| `data_kms_key_arn`                     | `cmk-data` from landing-zone                          |
| `lifecycle_noncurrent_expiration_days` | Delete non-current versions after N days (default 90) |
| `tags`                                 | Common tags                                           |

## Outputs

Published to SSM under `/eks-agent-platform/<environment>/model-artifacts/`:

- `bucket_arn`, `bucket_name` (artifacts)
- `eval_reports_bucket_arn`, `eval_reports_bucket_name`

## Consumed by

- The operator (when reconciling `Platform` CRs, grants tenant IRSA roles `s3:Get*` / `s3:Put*` on `tenants/<platform-id>/*`)
- `eval-controller` reads/writes to `eval_reports_bucket`
- Grafana datasource (Athena) is wired against `eval_reports_bucket` in `eks-gitops/dashboards/`
