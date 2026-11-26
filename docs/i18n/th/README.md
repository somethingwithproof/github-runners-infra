> [English](../../../README.md)

# ตัวรัน JIT สำหรับ GitHub Actions แบบมัลติคลาวด์

[![CI](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/somethingwithproof/github-runners-infra/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](../../../LICENSE)

ตัวควบคุมที่เขียนด้วย Go ซึ่งสร้างตัวรัน GitHub Actions ชั่วคราวแบบใช้ครั้งเดียวต่องานและจำกัดขอบเขตระดับคลังโค้ดบน DigitalOcean, AWS EC2, Google Compute Engine หรือ Azure VMs

## สถาปัตยกรรมความปลอดภัย

- ตรวจสอบลายเซ็น HMAC ของเว็บฮุก รหัสการติดตั้ง รหัสการส่งมอบ และรายการคลังโค้ดที่อนุญาต
- จัดเก็บข้อมูลการส่งมอบอย่างถาวรก่อนตอบรับ และตรวจสอบผ่าน GitHub Actions API ว่างานยังคงอยู่ในคิวก่อนการจัดเตรียมทรัพยากร
- สร้างการตั้งค่าตัวรัน JIT ของ GitHub แบบใช้ครั้งเดียวในขอบเขตระดับคลังโค้ด และติดแท็กความเป็นเจ้าของของตัวควบคุมให้กับ VM ทุกเครื่องก่อนการสร้าง
- ลบเฉพาะอินสแตนซ์ที่ตัวควบคุมเป็นเจ้าของหลังจากงานเสร็จสิ้นหรือเมื่อหมดอายุ TTL

## ผู้ให้บริการที่รองรับ

| `COMPUTE_PROVIDER` | ความจุแบบมาตรฐาน | ความจุแบบขัดจังหวะได้ |
| --- | --- | --- |
| `digitalocean` | Droplet | ไม่รองรับ (`RUNNER_SPOT=true` จะล้มเหลวอย่างปลอดภัย) |
| `aws` | EC2 On-Demand | EC2 one-time Spot |
| `gcp` | Standard VM | Spot VM |
| `azure` | Standard VM | Spot VM |

## การรองรับภาษาท้องถิ่น

ไบนารี `webhook` และ `installation-router` ฝังคำแปลสำหรับ 45 ภาษาท้องถิ่น (ภาษาท้องถิ่นระดับ Tier 1 จำนวน 20 ภาษา แอฟริกา 15 ภาษา และเอเชีย 10 ภาษา) ไว้ใน `internal/i18n` การเลือกภาษาท้องถิ่นจะยึดตาม `LC_ALL`, `LC_MESSAGES`, `LANG` และ `--lang`:

```bash
./bin/webhook --lang de
LANG=ja_JP.UTF-8 ./bin/installation-router
```

## การพัฒนาและสัญญาอนุญาต

```bash
go test ./...
make build
```

ดูรายละเอียดการปรับใช้และการปฏิบัติงานทั้งหมดได้ที่ [README ภาษาอังกฤษ](../../../README.md) อนุญาตให้ใช้งานภายใต้สัญญาอนุญาต [MIT](../../../LICENSE)
