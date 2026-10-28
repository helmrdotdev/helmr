data "aws_availability_zones" "available" {
  state = "available"
}

resource "aws_vpc" "quickstart" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
}

resource "aws_internet_gateway" "quickstart" {
  vpc_id = aws_vpc.quickstart.id
}

resource "aws_subnet" "public" {
  vpc_id                  = aws_vpc.quickstart.id
  cidr_block              = cidrsubnet(var.vpc_cidr, 8, 0)
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.quickstart.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.quickstart.id
  }
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.quickstart.id
  service_name      = "com.amazonaws.${var.aws_region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [aws_route_table.public.id]
}

resource "aws_eip" "control" {
  domain = "vpc"
}

resource "aws_eip_association" "control" {
  instance_id   = aws_instance.control.id
  allocation_id = aws_eip.control.id
}

resource "aws_security_group" "control" {
  name   = "${local.name}-control"
  vpc_id = aws_vpc.quickstart.id
  ingress {
    description = "HTTPS"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  ingress {
    description = "ACME and HTTPS redirect"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}
