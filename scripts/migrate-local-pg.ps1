param(
    [string]$HostName = "localhost",
    [int]$Port = 5432,
    [string]$User = "postgres",
    [string]$Password = "postgres",
    [string]$Database = "b2b_pilot"
)

$psql = Get-Command psql -ErrorAction SilentlyContinue
if (-not $psql) {
    throw "psql was not found. Install PostgreSQL and add its bin directory to PATH."
}

$env:PGPASSWORD = $Password
$baseArgs = @("-h", $HostName, "-p", "$Port", "-U", $User)

$exists = & $psql.Source @baseArgs "-d" "postgres" "-tAc" "SELECT 1 FROM pg_database WHERE datname='$Database'" 2>&1
if ($LASTEXITCODE -ne 0) {
    throw "Unable to connect to PostgreSQL at $HostName`:$Port as $User`n$exists"
}

if (($exists | Out-String).Trim() -ne "1") {
    & $psql.Source @baseArgs "-d" "postgres" "-v" "ON_ERROR_STOP=1" "-c" "CREATE DATABASE $Database;"
    if ($LASTEXITCODE -ne 0) {
        throw "Failed to create database $Database"
    }
}

$dsnArgs = $baseArgs + @("-d", $Database, "-v", "ON_ERROR_STOP=1")
Get-ChildItem -LiteralPath (Join-Path $PSScriptRoot "..\migrations") -Filter "*.sql" -File |
    Sort-Object Name |
    ForEach-Object {
        Write-Host "Applying $($_.Name)"
        & $psql.Source @dsnArgs "-f" $_.FullName
        if ($LASTEXITCODE -ne 0) {
            throw "Migration failed: $($_.Name)"
        }
    }

Write-Host "Local PostgreSQL migrations completed for $Database@$HostName`:$Port"
