/*
 * VPC layout (v0):
 *
 *   vpc_cidr (10.0.0.0/16)
 *   ├─ public/<az0>   10.0.1.0/24   ALB
 *   ├─ public/<az1>   10.0.2.0/24   ALB
 *   ├─ private/<az0>  10.0.10.0/24  Fargate task + RDS
 *   └─ private/<az1>  10.0.11.0/24  Fargate task + RDS
 *
 * One IGW for public-subnet egress. One NAT gateway in az0 only —
 * v1+ should add a second NAT for HA, but the per-month cost (~$32
 * + per-GB egress) doubles for a v0 traffic profile that doesn't
 * justify it. If az0 is impaired, the private subnets in az1 lose
 * outbound internet (RDS keeps working internally). Operators can
 * toggle to multi-AZ NAT by changing the count below.
 */

resource "aws_vpc" "main" {
  cidr_block           = var.vpc_cidr
  enable_dns_hostnames = true
  enable_dns_support   = true

  tags = {
    Name = "${var.project}-${var.environment}"
  }
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id

  tags = {
    Name = "${var.project}-${var.environment}-igw"
  }
}

# Public subnets — ALB lives here. /24 each (251 usable hosts) is
# orders of magnitude beyond what a single ALB needs but matches the
# /16 VPC boundary cleanly.
resource "aws_subnet" "public" {
  count = length(var.availability_zones)

  vpc_id                  = aws_vpc.main.id
  availability_zone       = var.availability_zones[count.index]
  cidr_block              = cidrsubnet(var.vpc_cidr, 8, count.index + 1)
  map_public_ip_on_launch = true

  tags = {
    Name = "${var.project}-${var.environment}-public-${var.availability_zones[count.index]}"
    Tier = "public"
  }
}

# Private subnets — Fargate tasks + RDS. Outbound traffic egresses
# via the NAT in public/az0. RDS Multi-AZ failover (v1+) needs a
# subnet in each AZ, hence two private subnets even though we only
# spin up a single-AZ db instance for v0.
resource "aws_subnet" "private" {
  count = length(var.availability_zones)

  vpc_id            = aws_vpc.main.id
  availability_zone = var.availability_zones[count.index]
  cidr_block        = cidrsubnet(var.vpc_cidr, 8, count.index + 10)

  tags = {
    Name = "${var.project}-${var.environment}-private-${var.availability_zones[count.index]}"
    Tier = "private"
  }
}

# EIP + single NAT gateway in public/az0. Cost-optimized for v0.
resource "aws_eip" "nat" {
  domain = "vpc"

  tags = {
    Name = "${var.project}-${var.environment}-nat"
  }

  depends_on = [aws_internet_gateway.main]
}

resource "aws_nat_gateway" "main" {
  allocation_id = aws_eip.nat.id
  subnet_id     = aws_subnet.public[0].id

  tags = {
    Name = "${var.project}-${var.environment}-nat"
  }

  depends_on = [aws_internet_gateway.main]
}

# Public route table — direct internet egress via IGW.
resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }

  tags = {
    Name = "${var.project}-${var.environment}-public"
  }
}

resource "aws_route_table_association" "public" {
  count = length(aws_subnet.public)

  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# Private route table — outbound egress via NAT.
resource "aws_route_table" "private" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.main.id
  }

  tags = {
    Name = "${var.project}-${var.environment}-private"
  }
}

resource "aws_route_table_association" "private" {
  count = length(aws_subnet.private)

  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private.id
}
