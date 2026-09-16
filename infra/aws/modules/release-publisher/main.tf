locals {
  name = lower(var.name)
}

resource "aws_iam_role" "platform_publisher" {
  name = "${local.name}-platform-publisher"
  tags = var.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        AWS = var.principal_arns
      }
      Action = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "platform_publisher" {
  name = "${local.name}-platform-publisher"
  role = aws_iam_role.platform_publisher.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
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
            "kms:ViaService" = "s3.${data.aws_region.current.region}.amazonaws.com"
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
    ]
  })
}
