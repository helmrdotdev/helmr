resource "aws_iam_role" "control" {
  name = "${local.name}-control"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "ec2.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
}

resource "aws_iam_instance_profile" "control" {
  name = "${local.name}-control"
  role = aws_iam_role.control.name
}

resource "aws_iam_role_policy_attachment" "control_ssm" {
  role       = aws_iam_role.control.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_role_policy" "control" {
  role = aws_iam_role.control.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["secretsmanager:GetSecretValue"]
        Resource = [for key, secret in aws_secretsmanager_secret.helmr : secret.arn if key != "checkpoint_encryption_key"]
      },
      {
        Effect    = "Allow"
        Action    = ["kms:Decrypt"]
        Resource  = aws_kms_key.helmr.arn
        Condition = { StringEquals = { "kms:ViaService" = "secretsmanager.${var.aws_region}.amazonaws.com" } }
      },
      {
        Effect = "Allow"
        Action = ["s3:GetObject", "s3:PutObject", "s3:PutObjectTagging", "s3:DeleteObject",
          "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts", "s3:ListBucketMultipartUploads",
        "s3:ListBucketVersions", "s3:DeleteObjectVersion", "s3:ListBucket"]
        Resource = [aws_s3_bucket.cas.arn, "${aws_s3_bucket.cas.arn}/*"]
      },
      {
        Effect    = "Allow"
        Action    = ["kms:Decrypt", "kms:Encrypt", "kms:GenerateDataKey"]
        Resource  = aws_kms_key.helmr.arn
        Condition = { StringEquals = { "kms:ViaService" = "s3.${var.aws_region}.amazonaws.com" } }
      },
      {
        Effect   = "Allow"
        Action   = ["s3:GetObject"]
        Resource = "${var.platform_store_bucket_arn}/objects/sha256/*"
      },
      {
        Effect    = "Allow"
        Action    = ["kms:Decrypt"]
        Resource  = var.platform_store_kms_key_arn
        Condition = { StringEquals = { "kms:ViaService" = "s3.${var.aws_region}.amazonaws.com" } }
      }
    ]
  })
}
