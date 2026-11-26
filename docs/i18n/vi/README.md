> [English](../../../README.md)

# GitHub Actions JIT Runner đa đám mây

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

Một bộ điều khiển Go giúp khởi tạo các GitHub Actions runner tạm thời, dùng cho một tác vụ duy nhất và giới hạn trong phạm vi kho lưu trữ trên DigitalOcean, AWS EC2, Google Compute Engine hoặc máy ảo Azure.

## Kiến trúc bảo mật

- Xác minh chữ ký HMAC của webhook, ID cài đặt, ID phân phối và danh sách kho lưu trữ được phép.
- Lưu trữ bền vững các sự kiện phân phối trước khi xác nhận đã nhận và xác minh qua GitHub Actions API rằng tác vụ vẫn đang trong hàng đợi trước khi cấp phát tài nguyên.
- Tạo cấu hình GitHub JIT runner dùng một lần trong phạm vi kho lưu trữ và gắn thẻ quyền sở hữu của bộ điều khiển cho mọi máy ảo trước khi khởi tạo.
- Chỉ xóa các thực thể thuộc quyền sở hữu của bộ điều khiển sau khi tác vụ hoàn tất hoặc hết thời gian tồn tại (TTL).

## Nhà cung cấp được hỗ trợ

| `COMPUTE_PROVIDER` | Tài nguyên tiêu chuẩn | Tài nguyên có thể bị thu hồi |
| --- | --- | --- |
| `digitalocean` | Droplet | Không khả dụng (`RUNNER_SPOT=true` sẽ từ chối an toàn) |
| `aws` | EC2 On-Demand | EC2 Spot dùng một lần |
| `gcp` | VM tiêu chuẩn | Spot VM |
| `azure` | VM tiêu chuẩn | Spot VM |

## Bản địa hóa

Các tệp nhị phân `webhook` và `installation-router` nhúng sẵn bản dịch cho 45 ngôn ngữ địa phương (20 ngôn ngữ Bậc 1, 15 ngôn ngữ châu Phi và 10 ngôn ngữ châu Á) trong `internal/i18n`. Việc chọn ngôn ngữ tuân theo `LC_ALL`, `LC_MESSAGES`, `LANG` và `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## Phát triển và Giấy phép

```bash
go test ./...
make build
```

Xem đầy đủ [README tiếng Anh](../../../README.md) để biết chi tiết về triển khai và vận hành. Phát hành theo giấy phép [MIT](../../../LICENSE).
