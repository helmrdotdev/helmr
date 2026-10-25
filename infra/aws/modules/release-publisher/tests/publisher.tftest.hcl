mock_provider "aws" {
  mock_data "aws_region" { defaults = { region = "us-east-1" } }
}
variables {
  name                                = "helmr-test"
  principal_arns                      = ["arn:aws:iam::000000000000:role/installation-publisher"]
  platform_store_bucket_arn           = "arn:aws:s3:::platform-artifacts"
  platform_store_kms_key_arn          = "arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000001"
  controlplane_release_repository_arn = "arn:aws:ecr:us-east-1:000000000000:repository/helmr/controlplane-releases"
}
run "exact_trust_and_platform_scope" {
  command = plan
  assert {
    condition = jsondecode(aws_iam_role.platform_publisher.assume_role_policy) == {
      Version   = "2012-10-17"
      Statement = [{ Effect = "Allow", Principal = { AWS = var.principal_arns }, Action = "sts:AssumeRole" }]
    }
    error_message = "Publisher trust must contain only the selected role/user principals."
  }
  assert {
    condition = (
      strcontains(aws_iam_role_policy.platform_publisher.policy, "${var.platform_store_bucket_arn}/objects/sha256/*") &&
      !strcontains(aws_iam_role_policy.platform_publisher.policy, "/controlplane/")
    )
    error_message = "Platform publisher must be bounded to immutable content-addressed objects."
  }

  assert {
    condition     = one([for s in jsondecode(aws_iam_role_policy.platform_publisher.policy).Statement : s if s.Sid == "EncryptPlatformObjects"]).Resource == var.platform_store_kms_key_arn && one([for s in jsondecode(aws_iam_role_policy.platform_publisher.policy).Statement : s if s.Sid == "EncryptPlatformObjects"]).Condition.StringEquals["kms:ViaService"] == "s3.us-east-1.amazonaws.com"
    error_message = "Platform encryption must use only the selected key through regional S3."
  }
}
run "release_publisher_cannot_delete_controlplane_images" {
  command = apply

  assert {
    condition = (
      toset([for statement in jsondecode(aws_iam_role_policy.platform_publisher.policy).Statement : jsonencode(merge(statement, { Action = sort(flatten([statement.Action])), Resource = sort(flatten([statement.Resource])) }))]) ==
      toset([for statement in [
        {
          Sid      = "ListPlatformObjects"
          Effect   = "Allow"
          Action   = ["s3:ListBucket"]
          Resource = var.platform_store_bucket_arn
          Condition = {
            StringLike = {
              "s3:prefix" = [
                "objects/sha256",
                "objects/sha256/*"
              ]
            }
          }
        },
        {
          Sid    = "CreateAndVerifyPlatformObjects"
          Effect = "Allow"
          Action = [
            "s3:GetObject",
            "s3:GetObjectVersion",
            "s3:PutObject",
            "s3:AbortMultipartUpload",
            "s3:ListMultipartUploadParts"
          ]
          Resource = "${var.platform_store_bucket_arn}/objects/sha256/*"
        },
        {
          Sid    = "EncryptPlatformObjects"
          Effect = "Allow"
          Action = [
            "kms:Decrypt",
            "kms:Encrypt",
            "kms:GenerateDataKey"
          ]
          Resource = var.platform_store_kms_key_arn
          Condition = {
            StringEquals = {
              "kms:ViaService" = "s3.us-east-1.amazonaws.com"
            }
          }
        },
        {
          Sid      = "AuthenticateControlPlaneReleaseRegistry"
          Effect   = "Allow"
          Action   = ["ecr:GetAuthorizationToken"]
          Resource = "*"
        },
        {
          Sid    = "PublishAndVerifyControlPlaneReleaseImages"
          Effect = "Allow"
          Action = [
            "ecr:BatchCheckLayerAvailability",
            "ecr:BatchGetImage",
            "ecr:CompleteLayerUpload",
            "ecr:DescribeImages",
            "ecr:DescribeRepositories",
            "ecr:InitiateLayerUpload",
            "ecr:PutImage",
            "ecr:UploadLayerPart"
          ]
          Resource = var.controlplane_release_repository_arn
        }
      ] : jsonencode(merge(statement, { Action = sort(flatten([statement.Action])), Resource = sort(flatten([statement.Resource])) }))])
    )
    error_message = "release publisher authority must stop at publish and verification."
  }
}

run "empty_principals_rejected" {
  command = plan
  variables { principal_arns = [] }
  expect_failures = [var.principal_arns]
}
run "non_iam_principal_rejected" {
  command = plan
  variables { principal_arns = ["arn:aws:sts::000000000000:assumed-role/publisher/session"] }
  expect_failures = [var.principal_arns]
}

run "wildcard_resources_rejected" {
  command = plan
  variables {
    platform_store_bucket_arn           = "arn:aws:s3:::*"
    platform_store_kms_key_arn          = "arn:aws:kms:us-east-1:000000000000:key/*"
    controlplane_release_repository_arn = "arn:aws:ecr:us-east-1:000000000000:repository/*"
  }
  expect_failures = [var.platform_store_bucket_arn, var.platform_store_kms_key_arn, var.controlplane_release_repository_arn]
}
