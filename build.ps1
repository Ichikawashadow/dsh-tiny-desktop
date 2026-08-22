# ============================================================
# dsh-tiny-desktop 一键构建脚本
# 用法: ./build.ps1   → 输出: DSH Tray.exe（单文件，零外部依赖）
# 环境要求: Go 1.21+（首次运行会自动下载 rsrc 工具）
# 可在任意目录执行（路径基于脚本自身位置解析）
# ============================================================
$ErrorActionPreference = "Stop"
$root = $PSScriptRoot

# 可选：国内网络加速
$env:GOPROXY = "https://goproxy.cn,direct"
$env:GOSUMDB = "off"

Push-Location $root
try {
    Write-Host "==> 嵌入 exe 文件图标 (rsrc)..."
    go run github.com/akavel/rsrc@latest -ico "$root\assets\dsh-v9.ico" -o "$root\rsrc.syso"
    if ($LASTEXITCODE -ne 0) { Write-Host "rsrc 失败"; exit 1 }

    Write-Host "==> 编译 (winexe, 无控制台窗口)..."
    go build -ldflags "-H windowsgui" -o "$root\DSH Tray.exe" .
    if ($LASTEXITCODE -ne 0) { Write-Host "编译失败"; exit 1 }

    Copy-Item "$root\DSH Tray.exe" "$root\DSH.Tray.exe" -Force

    $size = [math]::Round((Get-Item "$root\DSH Tray.exe").Length / 1MB, 1)
    Write-Host ""
    Write-Host "构建完成: DSH Tray.exe ($size MB)"
    Write-Host "部署: 复制 DSH Tray.exe 到任意目录，双击运行即可。"
}
finally {
    Pop-Location
}
