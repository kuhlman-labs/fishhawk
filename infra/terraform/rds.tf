/*
 * RDS Postgres for fishhawkd.
 *
 * Single-AZ db.t4g.micro by default (v0). Multi-AZ is a flip on
 * var.db_multi_az; the subnet group already spans both private
 * subnets so failover doesn't need new resources. Storage
 * auto-scales between var.db_allocated_storage and
 * var.db_max_allocated_storage.
 *
 * Master password is RDS-managed: AWS creates a Secrets Manager
 * entry, rotates it on a schedule, and we read it via
 * `aws_db_instance.master_user_secret`. Terraform then assembles
 * the libpq URL (postgres://USER:PASS@HOST:5432/DB?sslmode=require)
 * and writes it into our existing `database_url` Secrets Manager
 * entry. Operators don't need to touch the password by hand; the
 * ECS task picks it up via the secrets array in ecs.tf.
 *
 * Trade-off: the assembled URL ends up in Terraform state. State
 * is in the foundation's S3 bucket with KMS encryption + restricted
 * access, which is acceptable for v0. A future hardening pass can
 * shift to a read-side fetch (e.g. an init-container that pulls
 * from RDS's managed secret directly).
 *
 * TLS: parameter group sets rds.force_ssl=1, so `sslmode=require`
 * in the connection string isn't optional.
 */

resource "aws_db_subnet_group" "main" {
  name       = "${var.project}-${var.environment}"
  subnet_ids = aws_subnet.private[*].id

  tags = {
    Name = "${var.project}-${var.environment}"
  }
}

resource "aws_db_parameter_group" "main" {
  name        = "${var.project}-${var.environment}"
  family      = "postgres${split(".", var.db_engine_version)[0]}"
  description = "fishhawkd Postgres parameters."

  parameter {
    name  = "rds.force_ssl"
    value = "1"
  }

  parameter {
    name         = "log_min_duration_statement"
    value        = "1000" # log statements > 1s; cheap visibility into slow queries
    apply_method = "pending-reboot"
  }

  tags = {
    Name = "${var.project}-${var.environment}"
  }
}

resource "aws_db_instance" "main" {
  identifier = "${var.project}-${var.environment}"

  # Engine + sizing
  engine         = "postgres"
  engine_version = var.db_engine_version
  instance_class = var.db_instance_class

  allocated_storage     = var.db_allocated_storage
  max_allocated_storage = var.db_max_allocated_storage
  storage_type          = "gp3"
  storage_encrypted     = true

  # Database name + master credentials. RDS manages the password
  # in Secrets Manager so we never see plaintext on the Terraform
  # apply path.
  db_name                     = "fishhawk"
  username                    = "fishhawk_admin"
  manage_master_user_password = true

  # Networking — private subnets only, no public access.
  db_subnet_group_name   = aws_db_subnet_group.main.name
  vpc_security_group_ids = [aws_security_group.rds.id]
  publicly_accessible    = false
  port                   = 5432

  parameter_group_name = aws_db_parameter_group.main.name

  # HA + backup posture
  multi_az                   = var.db_multi_az
  backup_retention_period    = var.db_backup_retention_days
  backup_window              = "07:00-08:00" # UTC, off-hours for east-coast US
  maintenance_window         = "sun:08:30-sun:09:30"
  auto_minor_version_upgrade = true

  # Safety
  deletion_protection = var.db_deletion_protection
  skip_final_snapshot = !var.db_deletion_protection
  final_snapshot_identifier = var.db_deletion_protection ? (
    "${var.project}-${var.environment}-final-${formatdate("YYYYMMDDhhmm", timestamp())}"
  ) : null

  apply_immediately = false # production-ready default; flip per-apply if needed

  tags = {
    Name = "${var.project}-${var.environment}"
  }

  lifecycle {
    # `final_snapshot_identifier` includes a timestamp so it
    # changes on every plan; the lifecycle ignore prevents that
    # from forcing replacement.
    ignore_changes = [final_snapshot_identifier]
  }
}

# Read the RDS-managed master password and assemble the libpq URL
# into our existing database_url secret. The version resource is
# the convergence point — every plan recomputes from the current
# managed secret + endpoint.
data "aws_secretsmanager_secret_version" "rds_master" {
  secret_id  = aws_db_instance.main.master_user_secret[0].secret_arn
  depends_on = [aws_db_instance.main]
}

resource "aws_secretsmanager_secret_version" "database_url" {
  secret_id = aws_secretsmanager_secret.database_url.id
  secret_string = format(
    "postgres://%s:%s@%s:%d/%s?sslmode=require",
    aws_db_instance.main.username,
    jsondecode(data.aws_secretsmanager_secret_version.rds_master.secret_string)["password"],
    aws_db_instance.main.address,
    aws_db_instance.main.port,
    aws_db_instance.main.db_name,
  )
}
