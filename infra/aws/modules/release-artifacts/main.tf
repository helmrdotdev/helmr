locals {
  needs_manifest = var.control_image_override == null || (var.resolve_worker_ami && var.worker_ami_id_override == null)
  manifest_url = var.manifest_url != null ? var.manifest_url : (
    var.manifest_base_url != null ? "${trimsuffix(var.manifest_base_url, "/")}/${var.helmr_version}/aws-artifacts.json" : null
  )
  can_fetch_manifest = local.needs_manifest && local.manifest_url != null
}

data "http" "manifest" {
  count = local.can_fetch_manifest ? 1 : 0

  url = coalesce(local.manifest_url, "https://example.invalid/helmr-release-manifest-not-used.json")

  request_headers = {
    Accept = "application/json"
  }
}

locals {
  manifest_status_code = local.can_fetch_manifest ? data.http.manifest[0].status_code : null
  manifest_json        = local.can_fetch_manifest ? try(jsondecode(data.http.manifest[0].response_body), null) : null

  manifest_control_image = local.manifest_json != null ? try(tostring(local.manifest_json.control_image), null) : null
  manifest_worker_ami_id = local.manifest_json != null ? try(tostring(local.manifest_json.worker_amis[var.aws_region]), null) : null

  control_image = var.control_image_override != null ? var.control_image_override : local.manifest_control_image
  worker_ami_id = var.worker_ami_id_override != null ? var.worker_ami_id_override : (
    var.resolve_worker_ami ? local.manifest_worker_ami_id : null
  )
}

resource "terraform_data" "resolved" {
  input = {
    control_image = local.control_image
    worker_ami_id = local.worker_ami_id
    manifest_url  = local.needs_manifest ? local.manifest_url : null
  }

  lifecycle {
    precondition {
      condition     = !local.needs_manifest || local.manifest_url != null
      error_message = "A release manifest is required. Set manifest_url or manifest_base_url, or provide overrides for all requested artifacts."
    }

    precondition {
      condition     = !local.can_fetch_manifest || local.manifest_status_code == 200
      error_message = "Unable to fetch release manifest. The manifest URL must return HTTP 200."
    }

    precondition {
      condition     = !local.can_fetch_manifest || local.manifest_json != null
      error_message = "Unable to parse release manifest. The manifest response must be valid JSON."
    }

    precondition {
      condition     = try(trimspace(local.control_image) != "", false)
      error_message = "Unable to resolve control_image. Provide a manifest with control_image or set control_image_override."
    }

    precondition {
      condition     = can(regex("@sha256:[0-9a-f]{64}$", local.control_image))
      error_message = "control_image must be pinned by digest using @sha256:<64 lowercase hex characters>."
    }

    precondition {
      condition     = !var.resolve_worker_ami || try(trimspace(local.worker_ami_id) != "", false)
      error_message = "Unable to resolve worker_ami_id for aws_region. Provide worker_amis[aws_region] in the manifest, set worker_ami_id_override, or set resolve_worker_ami to false."
    }

    precondition {
      condition     = local.worker_ami_id == null || can(regex("^ami-[0-9a-f]{8,}$", local.worker_ami_id))
      error_message = "worker_ami_id must match ^ami-[0-9a-f]{8,}$."
    }
  }
}
