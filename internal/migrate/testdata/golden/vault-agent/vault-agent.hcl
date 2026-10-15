# The host Vault Agent rendering CYSTEMD v4's VAULT_TEMPLATES, converted by
# `cystemd migrate`. Install as /etc/vault-agent/vault-agent.hcl with the
# templates in /etc/vault-agent/templates (deploy/systemd/vault-agent.service).

vault {
  address = "https://vault.example.com:8200"
}

auto_auth {
  method "approle" {
    mount_path = "auth/approle"
    config = {
      remove_secret_id_file_after_reading = false
      role_id_file_path                   = "/etc/vault-agent/role-id"
      secret_id_file_path                 = "/etc/vault-agent/secret-id"
    }
  }
}

template_config {
  static_secret_render_interval = "10m0s"
}

template {
  source               = "/etc/vault-agent/templates/app.ctmpl"
  destination          = "/etc/myapp/app.conf"
  perms                = "0600"
  command              = "systemctl reload myapp.service"
  error_on_missing_key = true
}

template {
  source               = "/etc/vault-agent/templates/missing.ctmpl"
  destination          = "/etc/other/other.conf"
  perms                = "0600"
  error_on_missing_key = true
}

template {
  source               = "/etc/vault-agent/templates/other.ctmpl"
  destination          = "/etc/other/second.conf"
  perms                = "0600"
  error_on_missing_key = true
}
