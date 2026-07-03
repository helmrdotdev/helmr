output "clickhouse_url" {
  description = "Private ClickHouse HTTPS URL for HELMR_CLICKHOUSE_URL."
  value       = "https://${local.private_dns_hostname_id}:${var.https_port}"
}

output "clickhouse_user" {
  description = "ClickHouse user for HELMR_CLICKHOUSE_USER."
  value       = "default"
}

output "clickhouse_password_secret_arn" {
  description = "Secrets Manager ARN containing HELMR_CLICKHOUSE_PASSWORD."
  value       = aws_secretsmanager_secret.clickhouse_password.arn
  depends_on  = [aws_secretsmanager_secret_version.clickhouse_password]
}

output "clickhouse_password_kms_key_id" {
  description = "KMS key ID or ARN used for the ClickHouse password secret, when configured."
  value       = var.secret_kms_key_id
}

output "client_security_group_id" {
  description = "Security group to attach to Helmr clients that connect to ClickHouse Cloud."
  value       = aws_security_group.client.id
}

output "endpoint_security_group_id" {
  description = "Security group attached to the ClickHouse Cloud interface endpoint."
  value       = aws_security_group.endpoint.id
}

output "vpc_endpoint_id" {
  description = "AWS interface endpoint ID attached to the ClickHouse Cloud service."
  value       = aws_vpc_endpoint.clickhouse.id
}

output "private_dns_hostname" {
  description = "ClickHouse Cloud private DNS hostname."
  value       = local.private_dns_hostname_id
}

output "service_id" {
  description = "ClickHouse Cloud service ID."
  value       = clickhouse_service.telemetry.id
}
