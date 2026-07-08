# Dung moi instance Kiro-Go dang chay truoc khi khoi chay ban moi.
#
# Ly do: server chi doc data/config.json MOT LAN luc khoi dong vao RAM, sau do
# cu moi 30s (va luc thoat) lai ghi ban trong RAM de len file (backgroundStatsSaver
# -> config.Save). Neu ban sua tay config.json (vd them API key moi) trong khi mot
# instance cu con chay, ban RAM cua no KHONG co key moi -> no ghi de -> key bi mat.
#
# Buoc nay TERMINATE (kill cung) instance cu. Kill cung nen server cu KHONG chay
# save-on-exit -> file config.json vua sua tay tren dia duoc giu nguyen, va instance
# moi se doc dung noi dung do.
#
# An toan khi khong co gi dang chay (khong bao loi).

$ErrorActionPreference = 'SilentlyContinue'

$killed = 0

# Binary server ten la kiro-go.exe ca khi build (go build) lan khi chay qua
# `go run .` (module = kiro-go -> exe tam cung ten kiro-go.exe). Day la tien
# trinh duy nhat giu socket cong va ghi de config.json.
$procs = Get-Process -Name 'kiro-go' -ErrorAction SilentlyContinue
foreach ($p in $procs) {
    try {
        Stop-Process -Id $p.Id -Force -ErrorAction Stop
        Write-Host "[i] Da dung server cu (kiro-go.exe PID $($p.Id))" -ForegroundColor Cyan
        $killed++
    } catch {}
}

if ($killed -gt 0) {
    # Cho OS nha cong TCP truoc khi instance moi bind lai.
    Start-Sleep -Milliseconds 700
    Write-Host "[i] Da dung $killed instance cu." -ForegroundColor Cyan
} else {
    Write-Host "[i] Khong co server cu nao dang chay." -ForegroundColor DarkGray
}
