param(
    [string]$DbHost = "localhost",
    [int]$Port = 5432,
    [string]$User = "postgres",
    [string]$Password = "postgres",
    [string]$Database = "b2b_pilot",
    [string]$Psql = "$env:LOCALAPPDATA\PostgreSQL\16\pgsql\bin\psql.exe"
)

$ErrorActionPreference = "Stop"
if (-not (Test-Path -LiteralPath $Psql)) {
    throw "psql not found: $Psql"
}

$env:PGPASSWORD = $Password
$sql = @"
INSERT INTO tenants (id, name)
VALUES ('chang-an', U&'\957F\5B89\4EC1\6052')
ON CONFLICT (id) DO NOTHING;

INSERT INTO users (id, tenant_id, name, role, wecom_user_id)
VALUES ('u-sales', 'chang-an', U&'\9500\552E-\5F20\4E09', 'SALES', 'wecom_sales')
ON CONFLICT (id) DO UPDATE
SET tenant_id = EXCLUDED.tenant_id,
    name = EXCLUDED.name,
    role = EXCLUDED.role,
    wecom_user_id = EXCLUDED.wecom_user_id;
"@

& $Psql -h $DbHost -p $Port -U $User -d $Database -v ON_ERROR_STOP=1 -c $sql
if ($LASTEXITCODE -ne 0) {
    throw "local sales seed failed with exit code $LASTEXITCODE"
}

Write-Output "Local demo sales persons seeded in $Database@$($DbHost):$Port"
