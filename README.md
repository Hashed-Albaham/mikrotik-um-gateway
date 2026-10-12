# um-status-gateway

خدمة صغيرة بملف واحد، تعمل عند الطلب فقط: تفتح جلسة على بوابة **اليوزر منجر** في MikroTik RouterOS v7، تقرأ بيانات الكرت، تحسب الرصيد المتبقي والوقت والصلاحية، وتعيد النتيجة بصيغة JSON جاهزة لصفحة الهوتسبوت.

لا حلقات خلفية، ولا ذاكرة مؤقتة، ولا جلسات مشتركة: كل طلب يفتح جلسته الخاصة ثم تُغلق بعد مدة قصيرة. الحجم النهائي للصورة أقل من 7 ميجابايت، والذاكرة المستهلكة تبقى حول 9 ميجابايت.

> هذه هي النسخة المبسطة. النسخة الموسّعة السابقة (اختبارات أكثر، حقول أرقام خام، أدوات قياس) محفوظة كما هي في فرع `extended`.

## طريقة العمل

```
العميل ──► GET /api/user-status?user=...&pass=...
             │
             ├─ 1) POST /um/api/login          (بيانات الدخول بصيغة نموذج)
             ├─ 2) GET  /um/api/getUser        ─┐ قراءتان متوازيتان
             ├─ 3) GET  /um/api/getUserProfiles ┘
             ├─ 4) حساب المتبقي وإخفاء رقم الكرت
             └─ 5) إرجاع JSON فوراً
                 ... ثم بعد 60 ثانية: POST /um/api/logout
```

## الاستخدام

```bash
curl "http://192.168.88.1:8080/api/user-status?user=0018067158&pass=1234"
```

| المرجع | إلزامي | الوصف |
|---|---|---|
| `user` أو `username` | نعم | رقم الكرت |
| `pass` أو `password` | لا | كلمة المرور |

يمكن إرسال القيم في الرابط أو في جسم طلب `POST` (الصيغة `application/x-www-form-urlencoded`) لإبعاد كلمة المرور عن سجلات الوصول. ويوجد `/api/status` كاسم بديل، و`/healthz` لفحص الجاهزية.

### الاستجابة

```json
{
  "success": true,
  "masked_username": "00******58",
  "profile_name": "1000-1G-8h",
  "profile_state": "نشط",
  "data_left": "874 MB",
  "total_data": "1 GB",
  "time_left": "6h 30m",
  "total_time": "8h",
  "remaining_validity": "6h 30m",
  "used_download": "100M",
  "used_upload": "50M",
  "used_uptime": "1h30m",
  "active_sessions": 2,
  "shared_users": 1
}
```

| الحقل | المعنى |
|---|---|
| `masked_username` | رقم الكرت مع إخفاء الوسط؛ الرقم الكامل لا يُعاد أبداً |
| `profile_state` | `نشط` أو `منتهي` أو `غير معروف` |
| `data_left` / `total_data` | المتبقي من الرصيد / إجمالي الرصيد (`غير محدود` للخطط المفتوحة) |
| `time_left` / `total_time` | الوقت المتبقي / إجمالي وقت الخطة |
| `remaining_validity` | الصلاحية المتبقية كما تعيدها البوابة |
| `used_download` / `used_upload` / `used_uptime` | ما استُهلك حتى الآن |
| `active_sessions` / `shared_users` | الجلسات الحالية / عدد المستخدمين المسموح |

عند الخطأ تكون الاستجابة بصيغة موحدة: `{"success": false, "error": "..."}` برمز `400` إذا كان رقم الكرت ناقصاً، و`401` إذا رفضت البوابة بيانات الدخول، و`502` إذا لم تصل الخدمة إلى البوابة.

## الإعدادات

كل شيء من متغيرات البيئة، وكلها اختيارية:

| المتغير | الافتراضي | الوصف |
|---|---|---|
| `UM_TARGET_URL` | – | رابط البوابة كاملاً، مثل `http://192.168.216.1:12008` (له الأولوية) |
| `UM_PROTO` | `http` | البروتوكول عند استخدام الأجزاء الثلاثة |
| `UM_HOST` | `192.168.216.1` | عنوان البوابة |
| `UM_PORT` | `12008` | بورت البوابة |
| `LISTEN_ADDR` | `:8080` | منفذ الخدمة، ويمكن استخدام `PORT` بدلاً منه |
| `LOGOUT_DELAY_SECONDS` | `60` | التأخير قبل إغلاق جلسة البوابة |
| `REQUEST_TIMEOUT_SECONDS` | `8` | مهلة تسجيل الدخول والقراءة |
| `UM_INSECURE_SKIP_VERIFY` | `true` | اجعلها `false` لإلزام التحقق من شهادة البوابة |

## التركيب على الراوتر

```routeros
# 1. الشبكة الخاصة بالحاوية
/interface/veth/add name=veth1 address=172.17.0.2/24 gateway=172.17.0.1
/interface/bridge/add name=containers
/ip/address/add address=172.17.0.1/24 interface=containers
/interface/bridge/port/add bridge=containers interface=veth1
/ip/firewall/nat/add chain=srcnat action=masquerade src-address=172.17.0.0/24

# 2. متغيرات البيئة: عدّل العنوان والبورت حسب بوابتك
/container/envs/add list=ENV_UMGW key=UM_PROTO value="http"
/container/envs/add list=ENV_UMGW key=UM_HOST value="192.168.216.1"
/container/envs/add list=ENV_UMGW key=UM_PORT value="12008"
/container/envs/add list=ENV_UMGW key=LISTEN_ADDR value=":8080"

# 3. سحب الحاوية وتشغيلها
/container/config/set registry-url=https://ghcr.io tmpdir=disk1/tmp memory-high=64M
/container/add remote-image=ghcr.io/hashed-albaham/mikrotik-um-gateway:latest \
    interface=veth1 root-dir=disk1/images/umgw envlist=ENV_UMGW name=umgw \
    start-on-boot=yes logging=yes
/container/print
/container/start umgw

# 4. السماح لصفحة الهوتسبوت بالوصول للخدمة
/ip/hotspot/walled-garden/ip/add action=accept protocol=tcp dst-address=172.17.0.2 dst-port=8080 comment="UM status"

# 5. اختياري: نشر الخدمة على عنوان الراوتر أيضاً
/ip/firewall/nat/add chain=dstnat action=dstnat protocol=tcp dst-address=192.168.88.1 \
    dst-port=8080 to-addresses=172.17.0.2 to-ports=8080
```

أوامر المتابعة: `/container/print` و`/container/log/print` و`/container/stop umgw` و`/container/update umgw`.

إذا كانت الحزمة على GHCR خاصة، أضف بياناتها: `/container/config/set username=<user> password=<token>` أو اجعل الحزمة عامة من إعدادات المستودع.

### صفحة فحص الرصيد

`examples/hotspot-balance-check.html` جاهزة للصق في `login.html` أو `status.html`. عدّل فيها `API_BASE` ليشير إلى عنوان الخدمة كما يراه الجهاز (`http://172.17.0.2:8080`)، وتأكد من تنفيذ الخطوة 4 أعلاه.

## البناء

```bash
go build -o bin/um-status-gateway .

docker build -t um-status-gateway .
docker run --rm -p 8080:8080 -e UM_HOST=192.168.216.1 -e UM_PORT=12008 um-status-gateway
```

سير عمل GitHub Actions يبني الصورة تلقائياً لكل من `linux/amd64` و`linux/arm64` و`linux/arm/v7` ويرفعها إلى GHCR، ويفشل البناء إذا تجاوز حجم صورة `amd64` عشرة ميجابايت.

## ملاحظات فنية

- **تسجيل الدخول** يُرسل بصيغة نموذج (`username`, `password`) كما تتوقعه البوابة، والقراءتان لا ترسلان رقم الكرت إطلاقاً لأن البوابة تربطهما بالجلسة.
- **حالة البروفايل** رقم `3` تعني مستهلك/منتهي، وأي قيمة أخرى تعني نشط، مع دعم القيم النصية مثل `used` في بعض الإصدارات.
- **صيغة اسم البروفايل** هي `السعر-البيانات-الوقت` مثل `1000-1G-8h`: الوحدة الكبيرة `K/M/G/T` تعني بيانات، والصغيرة `w/d/h/m/s` تعني وقتاً، وكلمة `un` تعني غير محدود، والأرقام المجردة (السعر) تُتجاهل.
- **البروفايل المنتهي**: إذا كان الكرت يحمل أكثر من بروفايل، يُختار أول بروفايل غير منتهٍ؛ وإذا كانت كلها منتهية يُعاد `منتهي` مع رصيد متبقٍ يساوي `0`.
- **الذاكرة**: بعد القراءتين تُغلق الاتصالات الخاملة فوراً، وبعد الإغلاق المؤجل تُغلق جلسة العميل بالكامل، فلا يبقى أي أثر للطلب.
- **الخصوصية**: كلمة المرور لا تُسجّل ولا تُعاد، ورقم الكرت يظهر مقنّعاً في كل السجلات (`00******58`).
- **الشهادة الذاتية**: بوابة الراوتر عادةً تستخدم شهادة موقعة ذاتياً، لذلك التحقق معطّل افتراضياً؛ اضبط `UM_INSECURE_SKIP_VERIFY=false` إن كانت شهادتك موثوقة.

## الترخيص

MIT، انظر [LICENSE](LICENSE).
- **قياس فعلي**: 8.2 ميجابايت بعد الإقلاع، و9.4 ميجابايت بعد 50 طلباً، وحجم الملف التنفيذي 6.9 ميجابايت (قِيست على `linux/amd64` بـ `GOGC=20` و`GOMEMLIMIT=4MiB`).
