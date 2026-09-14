#!/bin/bash
# 模式二集群栈 PG 初始化：三库三 role（design/deployment-modes.md 2.1）。
# 各 role 只授本库权限，从部署层强制三库唯一归属。
# 仅在数据目录为空时执行（postgres 官方镜像 initdb.d 语义）。
set -e

psql_exec() {
  psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" "$@"
}

for name in agent replay upstream; do
  password_var="POSTGRES_${name^^}_PASSWORD"
  password="${!password_var:-$name}"

  # role（幂等：已存在则跳过）
  psql_exec <<EOSQL
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '$name') THEN
    CREATE ROLE $name LOGIN PASSWORD '$password';
  END IF;
END \$\$;
EOSQL

  # database（CREATE DATABASE 不能进 DO 块，用存在性守卫）
  if ! psql_exec -tAc "SELECT 1 FROM pg_database WHERE datname='$name'" | grep -q 1; then
    createdb --username "$POSTGRES_USER" "$name"
  fi

  # 本库权限（重建/重复授权无害）
  psql_exec --dbname "$name" <<EOSQL
GRANT ALL PRIVILEGES ON DATABASE $name TO $name;
GRANT ALL ON SCHEMA public TO $name;
EOSQL
done

echo "pg-init: created agent/replay/upstream databases and roles"
