locals {
  host_config = {
    aws_region               = var.aws_region
    app_origin               = var.app_origin
    region_id                = var.region_id
    worker_group_name        = var.worker_group_name
    github_oauth_client_id   = var.github_oauth_client_id
    slack_transport          = var.slack_transport
    platform_store_uri       = var.platform_store_uri
    cas_uri                  = "s3://${aws_s3_bucket.cas.bucket}"
    blocked_ipv4_cidrs       = local.worker_network_blocked_ipv4_cidrs
    private_business_cidrs   = var.worker_network_private_business_cidrs
    controlplane_image       = module.release_artifacts.controlplane_image
    postgres_image           = var.postgres_image
    clickhouse_image         = var.clickhouse_image
    caddy_image              = var.caddy_image
    controlplane_environment = var.controlplane_environment
    dispatcher_environment   = var.dispatcher_environment
    secret_arns = {
      for key, secret in aws_secretsmanager_secret.helmr : key => secret.arn
      if key != "checkpoint_encryption_key"
    }
  }
  cloud_config = {
    package_update = true
    packages       = ["docker.io", "docker-compose-v2", "python3", "openssl", "curl", "unzip", "ca-certificates"]
    write_files = concat([
      { path = "/etc/helmr/quickstart.json", permissions = "0600", content = jsonencode(local.host_config) },
      { path = "/usr/local/sbin/helmr-quickstart-configure", permissions = "0700", content = file("${path.module}/host/configure.py") },
      { path = "/usr/local/sbin/helmr-quickstart-start", permissions = "0700", content = file("${path.module}/host/start.sh") },
      { path = "/etc/helmr/Caddyfile", permissions = "0644", content = <<-CADDY
        {
          admin off
        }
        ${var.app_origin} {
          reverse_proxy 127.0.0.1:8080 {
            flush_interval -1
          }
        }
        CADDY
      },
      { path = "/etc/helmr/clickhouse.xml", permissions = "0644", content = <<-XML
        <clickhouse>
          <listen_host replace="replace">127.0.0.1</listen_host>
          <max_server_memory_usage>2147483648</max_server_memory_usage>
          <logger>
            <console>1</console>
            <log remove="remove"/>
            <errorlog remove="remove"/>
          </logger>
        </clickhouse>
        XML
      }
      ], [for unit in ["helmr.service", "helmr-audit.service", "helmr-audit.timer"] : {
        path = "/etc/systemd/system/${unit}", permissions = "0644", content = file("${path.module}/host/${unit}")
    }])
    runcmd = [
      ["sh", "-ec", "workdir=$(mktemp -d); trap 'rm -rf \"$workdir\"' EXIT; curl --fail --silent --show-error --location https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip -o \"$workdir/awscliv2.zip\"; unzip -q \"$workdir/awscliv2.zip\" -d \"$workdir\"; \"$workdir/aws/install\" --update"],
      ["install", "-d", "-m", "0750", "-g", "999", "/etc/helmr/postgres-tls"],
      ["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "3650", "-subj", "/CN=localhost", "-keyout", "/etc/helmr/postgres-tls/server.key", "-out", "/etc/helmr/postgres-tls/server.crt"],
      ["chown", "root:999", "/etc/helmr/postgres-tls/server.key"],
      ["chmod", "0640", "/etc/helmr/postgres-tls/server.key"],
      ["systemctl", "daemon-reload"],
      ["systemctl", "enable", "--now", "docker"]
    ]
  }
  control_user_data = base64gzip("#cloud-config\n${jsonencode(local.cloud_config)}")
}

resource "aws_instance" "control" {
  ami                         = var.control_ami_id
  instance_type               = var.control_instance_type
  subnet_id                   = aws_subnet.public.id
  vpc_security_group_ids      = [aws_security_group.control.id]
  associate_public_ip_address = true
  iam_instance_profile        = aws_iam_instance_profile.control.name
  user_data_base64            = local.control_user_data
  user_data_replace_on_change = true
  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }
  root_block_device {
    encrypted             = true
    volume_type           = "gp3"
    volume_size           = var.control_disk_gb
    delete_on_termination = true
  }
  tags       = { Name = "${local.name}-control" }
  depends_on = [aws_route_table_association.public, aws_iam_role_policy.control, aws_iam_role_policy_attachment.control_ssm]
  lifecycle {
    precondition {
      condition     = length(local.control_user_data) * 3 / 4 <= 16384
      error_message = "Compressed control-host user data exceeds EC2's 16 KiB limit."
    }
  }
}
