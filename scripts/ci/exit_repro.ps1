# Temporary (#257): how each way of ending `up` under the W6 scheduled task
# shows in up.log and in the task's LastTaskResult. Prints a report; never
# fails the job.
$ErrorActionPreference = 'Continue'
$root = Join-Path $env:RUNNER_TEMP 'sbexit'
$data = Join-Path $root 'switchboard-pc'
New-Item -ItemType Directory -Force "$data\bin", "$data\config" | Out-Null
go build -o "$data\bin\switchboard.exe" ./cmd/switchboard
if ($LASTEXITCODE -ne 0) { 'build failed'; exit 0 }
$port = 18090
$task = 'sb-exit-repro'
$agent = $env:ComSpec
@"
@echo off
set SWITCHBOARD_ADMIN_AUTH=builtin
set SWITCHBOARD_INSECURE_COOKIE=1
set "SWITCHBOARD_CONFIG_DIR=%~dp0config"
set "SWITCHBOARD_HOSTING_AGENT=$agent"
"%~dp0bin\switchboard.exe" up --data "%~dp0." --port $port >> "%~dp0up.log" 2>&1
"@ | Set-Content "$data\up.cmd" -Encoding Ascii

$con = Join-Path $root 'con.ps1'
@'
param([int]$target, [string]$what, [string]$out)
Add-Type @"
using System; using System.Runtime.InteropServices;
public static class Con {
  [DllImport("kernel32.dll", SetLastError=true)] public static extern bool FreeConsole();
  [DllImport("kernel32.dll", SetLastError=true)] public static extern bool AttachConsole(uint pid);
  [DllImport("kernel32.dll", SetLastError=true)] public static extern bool GenerateConsoleCtrlEvent(uint ev, uint group);
  [DllImport("kernel32.dll", SetLastError=true)] public static extern bool SetConsoleCtrlHandler(IntPtr h, bool add);
  [DllImport("kernel32.dll")] public static extern IntPtr GetConsoleWindow();
  [DllImport("user32.dll", SetLastError=true)] public static extern bool PostMessage(IntPtr h, uint msg, IntPtr w, IntPtr l);
}
"@
$r = @()
$r += 'free ' + [Con]::FreeConsole()
$ok = [Con]::AttachConsole([uint32]$target); $r += "attach $target $ok err $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
if ($what -eq 'break') {
  $r += 'ignore ' + [Con]::SetConsoleCtrlHandler([IntPtr]::Zero, $true)
  $ok = [Con]::GenerateConsoleCtrlEvent(1, 0); $r += "break $ok err $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
} else {
  $h = [Con]::GetConsoleWindow(); $r += "window $h"
  [IO.File]::WriteAllText($out, ($r -join "`n"))
  $ok = [Con]::PostMessage($h, 0x10, [IntPtr]::Zero, [IntPtr]::Zero); $r += "close $ok err $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
}
[IO.File]::WriteAllText($out, ($r -join "`n"))
Start-Sleep 2
'@ | Set-Content $con

function Register([switch]$restart) {
  $action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -WindowStyle Hidden -Command `"& '$data\up.cmd'`""
  $trigger = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"
  if ($restart) {
    $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -MultipleInstances IgnoreNew -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
  } else {
    $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -MultipleInstances IgnoreNew
  }
  Register-ScheduledTask -TaskName $task -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
}
function UpPid { (Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1).OwningProcess }
function LogLen { if (Test-Path "$data\up.log") { (Get-Content "$data\up.log").Count } else { 0 } }
function Starts { @(Select-String -Path "$data\up.log" -Pattern 'admin auth: ' -ErrorAction SilentlyContinue).Count }
function Start-Up {
  Start-ScheduledTask -TaskName $task
  for ($i = 0; $i -lt 60; $i++) { if (UpPid) { break }; Start-Sleep 1 }
  Start-Sleep 2
  $p = UpPid
  $procs = Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -match 'sbexit' -or $_.ProcessId -eq $p } | ForEach-Object { "  $($_.ProcessId) parent $($_.ParentProcessId) $($_.Name) session $($_.SessionId)" }
  "up pid $p"; $procs
  $p
}
function Report([string]$kind, [int]$mark) {
  for ($i = 0; $i -lt 30; $i++) { if ((Get-ScheduledTask -TaskName $task).State -eq 'Ready' -and -not (UpPid)) { break }; Start-Sleep 1 }
  $info = Get-ScheduledTaskInfo -TaskName $task
  $keeper = @(Get-CimInstance Win32_Process -Filter "Name = 'switchboard.exe'" | Where-Object { $_.CommandLine -match '__keeper' }).Count
  "RESULT $kind | task $((Get-ScheduledTask -TaskName $task).State) LastTaskResult 0x$('{0:X}' -f $info.LastTaskResult) | listener $([bool](UpPid)) | keeper processes $keeper"
  "up.log lines added:"
  Get-Content "$data\up.log" | Select-Object -Skip $mark | ForEach-Object { "  | $_" }
}
function Down { $p = UpPid; if ($p) { Stop-Process -Id $p -Force }; Stop-ScheduledTask -TaskName $task -ErrorAction SilentlyContinue; for ($i = 0; $i -lt 30; $i++) { if (-not (UpPid) -and (Get-ScheduledTask -TaskName $task).State -eq 'Ready') { break }; Start-Sleep 1 } }

Register
foreach ($kind in 'terminate-up', 'ctrl-break', 'ctrl-close', 'kill-cmd', 'kill-powershell', 'stop-task') {
  ">>> $kind"
  $mark = LogLen
  $out = Start-Up; $out | Select-Object -SkipLast 1; $p = $out[-1]
  if (-not $p) { "RESULT $kind | up did not start"; Get-Content "$data\up.log" -Tail 20; continue }
  switch ($kind) {
    'terminate-up' { Stop-Process -Id $p -Force }
    'ctrl-break' { $f = "$root\con-break.txt"; Start-Process powershell.exe -ArgumentList '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $con, $p, 'break', $f -WindowStyle Hidden -Wait; Get-Content $f -ErrorAction SilentlyContinue }
    'ctrl-close' { $f = "$root\con-close.txt"; Start-Process powershell.exe -ArgumentList '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $con, $p, 'close', $f -WindowStyle Hidden -Wait; Get-Content $f -ErrorAction SilentlyContinue }
    'kill-cmd' { Get-CimInstance Win32_Process -Filter "Name = 'cmd.exe'" | Where-Object { $_.CommandLine -match 'up\.cmd' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force } }
    'kill-powershell' { Get-CimInstance Win32_Process -Filter "Name = 'powershell.exe'" | Where-Object { $_.CommandLine -match 'up\.cmd' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force } }
    'stop-task' { Stop-ScheduledTask -TaskName $task }
  }
  Start-Sleep 12
  Report $kind $mark
  "up still running after it: $([bool](UpPid))"
  Down
}

">>> start-up error (port held)"
$mark = LogLen
$hold = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, $port); $hold.Start()
Start-Up | Out-Null
Start-Sleep 5
Report 'start-up error' $mark
$hold.Stop()
Down

">>> restart on failure: terminate up"
Register -restart
$mark = LogLen; $before = Starts
$out = Start-Up; $p = $out[-1]
Stop-Process -Id $p -Force
for ($i = 0; $i -lt 150; $i++) { if ((Starts) -gt $before + 1) { break }; Start-Sleep 1 }
Report 'restart after terminate' $mark
"RESULT restart after terminate: started again $([bool]((Starts) -gt $before + 1)) after ${i}s"
Down

">>> restart on failure: sbstop as W3 writes it (listener killed, wait for Ready)"
Register -restart
$mark = LogLen
$out = Start-Up; $before = Starts
Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue | ForEach-Object { Stop-Process -Id $_.OwningProcess -Force }
for ($i = 0; $i -lt 150; $i++) { if ((Starts) -gt $before) { break }; Start-Sleep 1 }
Report 'restart after sbstop' $mark
"RESULT restart after sbstop kill: started again $([bool]((Starts) -gt $before)) after ${i}s"
Down

">>> restart on failure: Stop-ScheduledTask"
Register -restart
$mark = LogLen
$out = Start-Up; $before = Starts
Stop-ScheduledTask -TaskName $task
for ($i = 0; $i -lt 150; $i++) { if ((Starts) -gt $before) { break }; Start-Sleep 1 }
Report 'restart after Stop-ScheduledTask' $mark
"RESULT restart after Stop-ScheduledTask: started again $([bool]((Starts) -gt $before)) after ${i}s"
Down

Get-CimInstance Win32_Process -Filter "Name = 'switchboard.exe'" | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
Unregister-ScheduledTask -TaskName $task -Confirm:$false -ErrorAction SilentlyContinue
exit 0
