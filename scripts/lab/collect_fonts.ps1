param([string]$Pairs = "$env:TEMP\dx_pairs_samples.json", [int]$Count = 10)
$env:SENTINEL_TRACE_ELEMENTS = '1'
Push-Location $PSScriptRoot
for ($i = 0; $i -lt $Count; $i++) {
  $out = node lab_driver.js $Pairs $i --diag 2>&1 | Select-String -Pattern '\[el div.*(fontFamily|fontSize|innerText)'
  $line = ($out | ForEach-Object { $_.Line -replace '^\[el div\.?s?t?y?l?e?\]? set ', '' }) -join ' | '
  "$i : $line"
}
Pop-Location
Remove-Item Env:\SENTINEL_TRACE_ELEMENTS
