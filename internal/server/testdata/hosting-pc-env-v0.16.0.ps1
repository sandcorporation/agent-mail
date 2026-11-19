# The W3 block that saves env.ps1, verbatim from docs/hosting-pc.md at v0.16.0
# (before sbstop and sbstarted). TestE2E_HostingDocWindows runs it to stand
# for an install made from those docs.
@"
`$data = '$data'
`$port = $port
`$sb = '$sb'
`$agent = '$agent'
"@ | Set-Content "$data\env.ps1"
@'
function sbpc {
  $env:SWITCHBOARD_CONFIG_DIR = "$data\config"; $env:DB_PATH = "$data\switchboard.db"
  try { & $sb @args } finally { Remove-Item Env:SWITCHBOARD_CONFIG_DIR, Env:DB_PATH -ErrorAction SilentlyContinue }
}
function New-SbToken { $b = New-Object byte[] 32; [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b); [Convert]::ToBase64String($b) }
function Test-SbHealth { try { (Invoke-WebRequest "http://127.0.0.1:$port/health" -UseBasicParsing -TimeoutSec 2).StatusCode -eq 200 } catch { $false } }
function sbwait([int]$seconds, [scriptblock]$ok) {
  for ($i = 0; $i -lt $seconds; $i++) { if (& $ok) { return }; Start-Sleep 1 }
  throw "timed out after ${seconds}s: $ok"
}
function Get-SbTotp([string]$secret) {
  $alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  $bits = -join ($secret.ToUpper().TrimEnd('=').ToCharArray() | ForEach-Object { [Convert]::ToString($alphabet.IndexOf($_), 2).PadLeft(5, '0') })
  $key = [byte[]]@(for ($i = 0; $i + 8 -le $bits.Length; $i += 8) { [Convert]::ToByte($bits.Substring($i, 8), 2) })
  $counter = [BitConverter]::GetBytes([int64][Math]::Floor([DateTimeOffset]::UtcNow.ToUnixTimeSeconds() / 30))
  if ([BitConverter]::IsLittleEndian) { [Array]::Reverse($counter) }
  $h = (New-Object Security.Cryptography.HMACSHA1 (, $key)).ComputeHash($counter)
  $o = $h[19] -band 15
  $n = (([int]($h[$o] -band 127) -shl 24) -bor ([int]$h[$o + 1] -shl 16) -bor ([int]$h[$o + 2] -shl 8) -bor [int]$h[$o + 3]) % 1000000
  '{0:D6}' -f $n
}
function sblogin {
  $admin = Get-Content "$data\admin.json" -Raw | ConvertFrom-Json
  $body = @{ name = 'pcadmin'; password = (Get-Content "$data\admin.password"); code = (Get-SbTotp $admin.secret) } | ConvertTo-Json
  Invoke-RestMethod -Method Post "http://127.0.0.1:$port/auth/login" -ContentType 'application/json' -Headers @{ 'X-Switchboard-Dashboard' = '1' } -Body $body -SessionVariable web | Out-Null
  $global:sbweb = $web
}
function sblist { $r = Invoke-RestMethod "http://127.0.0.1:$port/api/admin/terminals" -WebSession $global:sbweb; $r }
function sbget([string]$id) { sblist | Where-Object id -eq $id }
function sbterm { sblist | Select-Object id, @{ n = 'state'; e = { $_.runtime.state } }, @{ n = 'pid'; e = { $_.runtime.pid } }, @{ n = 'run'; e = { $_.runtime.run } }, agent_session_id, @{ n = 'slot'; e = { $_.slot.role } }, presence | Format-Table -AutoSize }
'@ | Add-Content "$data\env.ps1"
. "$data\env.ps1"
sbpc version
