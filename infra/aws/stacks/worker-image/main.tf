locals {
  tags = {
    Project     = "helmr"
    Environment = "worker-image"
    ManagedBy   = "terraform"
  }
}

module "worker_image" {
  source = "../../modules/worker-image"

  name                                 = var.name
  host_artifacts_bundle_s3_uri         = var.host_artifacts_bundle_s3_uri
  host_artifacts_bundle_object_arn     = var.host_artifacts_bundle_object_arn
  host_artifacts_bundle_digest         = var.host_artifacts_bundle_digest
  host_artifacts_bundle_kms_key_arn    = var.host_artifacts_bundle_kms_key_arn
  host_artifacts_manifest_digest       = var.host_artifacts_manifest_digest
  runtime_artifacts_bundle_s3_uri      = var.runtime_artifacts_bundle_s3_uri
  runtime_artifacts_bundle_object_arn  = var.runtime_artifacts_bundle_object_arn
  runtime_artifacts_bundle_digest      = var.runtime_artifacts_bundle_digest
  runtime_artifacts_bundle_kms_key_arn = var.runtime_artifacts_bundle_kms_key_arn
  runtime_artifacts_manifest_digest    = var.runtime_artifacts_manifest_digest
  parent_image                         = var.parent_image
  distribution_regions                 = var.distribution_regions
  ami_public                           = var.ami_public
  root_volume_encrypted                = var.root_volume_encrypted
  instance_types                       = var.instance_types
  subnet_id                            = var.subnet_id
  security_group_ids                   = var.security_group_ids
  root_volume_size_gb                  = var.root_volume_size_gb
  tags                                 = local.tags
}
