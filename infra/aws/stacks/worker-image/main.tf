locals {
  tags = {
    Project     = "helmr"
    Environment = "worker-image"
    ManagedBy   = "terraform"
  }
}

module "worker_image" {
  source = "../../modules/worker-image"

  name                      = var.name
  source_repository_url     = var.source_repository_url
  source_ref                = var.source_ref
  source_bundle_s3_uri      = var.source_bundle_s3_uri
  source_bundle_bucket_arn  = var.source_bundle_bucket_arn
  source_bundle_object_arn  = var.source_bundle_object_arn
  source_bundle_kms_key_arn = var.source_bundle_kms_key_arn
  parent_image              = var.parent_image
  distribution_regions      = var.distribution_regions
  ami_public                = var.ami_public
  root_volume_encrypted     = var.root_volume_encrypted
  instance_types            = var.instance_types
  subnet_id                 = var.subnet_id
  security_group_ids        = var.security_group_ids
  buildkit_slirp_cidr       = var.buildkit_slirp_cidr
  image_version             = var.image_version
  tags                      = local.tags
}
