# 卡网和充值交付配置

卡网统一管理游客浏览器会话、邮箱验证登录、付款和售后；充值系统仅接受一笔已付款订单的授权。首期接入固定 GPT Plus 商品，升级报价及其他套餐不自动上架。

## 必需配置

| 所属系统 | 配置 | 说明 |
|---|---|---|
| DNS/入口 | shop.aichongzhi.best | 卡网页面及 /api，同域 HTTPS；原 kawang 保留其他域名 |
| 卡网 | PUBLIC_ORIGIN | https://shop.aichongzhi.best |
| 卡网 | AUTH_SECRET、数据库应用密码 | 服务端生成，保存在 secrets，不能提交 |
| 两边 | FULFILLMENT_SECRET / COMMERCE_SECRET | 同一个随机 32 字节小写十六进制字符串；不是供应商 Key |
| 卡网 | FULFILLMENT_ENABLED=true | 需要 API、Worker 都配置 |
| 卡网 | FULFILLMENT_ORIGIN | https://relay.aichongzhi.best |
| 卡网 | FULFILLMENT_PRODUCT_MAP | `{"plus":{"plan":"chatgptplusplan","region":"PH"}}`，地区必须得到充值目录支持 |
| 充值 | COMMERCE_ORIGIN | https://shop.aichongzhi.best |
| 充值 | COMMERCE_SERVICE_USER_ID | 已配置卡台及代理的商户服务账号；不把该账号的登录凭证发给游客 |
| 卡网 | 支付通道 | PAYMENT_CHECKOUT_ENABLED=true；配置现有支付宝或支付网关的商户号、签名密钥、验签公钥，以及 PAYMENT_NOTIFY_ORIGIN；具体字段见 internal/config/payments.go。必须收到验签通过、金额匹配的通知才授权充值 |
| 卡网 | 邮件服务 | MAIL_ENABLED=true、RESEND_API_KEY、MAIL_FROM，以及已验证发件域名；用于验证码和游客换设备找回 |
| 充值 | 上游及资金 | 现有 SUBSCRIPTION_API_KEY、YIKA Key/Secret、固定服务器出口白名单、动态代理、CARD_GATEWAY_URL/TOKEN；供应商余额及商户服务账号积分足够 |

卡网收取客户款项，充值系统积分记在商户服务账号内部账上，不要求游客再购买一次积分。YIKA 卡片充值金额和小玩家的上游费用仍独立发生，需要商户备付。

## 访问和状态

- 游客凭浏览器 buyer 会话或邮箱验证码恢复后的 customer 会话访问卡网原订单。
- POST `/api/orders/:id/delivery-link` 校验订单归属及实际付款，交付票据五分钟有效、仅可兑换一次，放 URL fragment，兑换后页面清除。
- 充值页 `/deliver/` 得到独立 HttpOnly、Secure、SameSite=Strict 的订单 Cookie；不能访问充值后台、钱包或其他订单。
- 检测与提交必须是同一份 Session，检测十分钟内有效。浏览器不能修改套餐、地区、金额、商户账号或充值 UUID。
- 执行前回查卡网实际付款、售后和关单状态，并持久化执行锁。已开始的充值在结果未明确前不能退款。
- Worker 定期推进同一笔已存在的充值，拉取状态并持久化到卡网 fulfillment_orders；不依赖用户保持页面打开。未知结果不重开卡、不自动重提支付。
- 收款、交付和供应商费用各自保存；充值失败不等于 YIKA 自动退款，售后必须核对。

## 上线与回滚

迁移新增 034_fulfillment_bridge.sql；应用迁移后必须重新执行 grant-runtime。充值新增 0010_commerce_fulfillment.sql，VPS 入口 .local/api-dev.mjs 必须安装到运行位置 api-dev.mjs。

当前脱敏 gptibo 与服务器旧版本迁移不兼容，使用独立部署目录、独立数据库与卷，禁止覆盖旧数据库。关闭 FULFILLMENT_ENABLED 能停止新订单接入，但仍有未完成交付时不能直接停 Worker 或丢失交付映射。回滚保留数据库和所有加密密钥，先确认在途充值。

真实支付通道和生产邮件未配置之前，不应对外宣称可实际购买，也不应使用模拟付款标记代替验签通知。

## 本次部署验收（2026-10-19）

- 独立目录 `/opt/gptibo-shop`、独立 PostgreSQL 卷，旧 gptibo/kawang 数据均保留。
- shop 域名页面及目录接口返回 200；新卡网 API/Worker/数据库健康检查通过。
- 充值 `/deliver/` 返回 200；无订单授权访问交付接口返回 401。
- 双向 HMAC 验证通过：只查询不存在的检查订单，未开卡或扣款。
- 36 项充值/游客交付测试通过；Go 测试、前端路由契约及两个前端构建通过。
- 在独立、无外网 PostgreSQL 容器验证新迁移、运行角色权限、游客隔离和付款/售后授权限制。
- 生产收款尚未接入，生产发件服务尚未配置；目录没有开放可购买商品。尚未执行真实客户付款到订阅成功的线上验收。

支付宝官方通道可以按现有适配器配置；脱敏版其他网关地址仍是占位值，需要提供真实支付网关和接口文档后恢复适配，不能只填商户密钥就宣称接通。

## 游客交付完善（2026-10-19）

邮件与收款继续暂缓，保持真实购买入口关闭。本轮新增：

- 交付页五步进度、每十秒只读状态更新、复制销售订单、最后更新时间及过期授权指引。
- Session 修改后废弃旧检测结果；提交响应丢失时先读取持久化订单，不自动重提；任务开始后清除页面 Session。
- 区分商户内部积分回退与客户原路退款，不将内部额度变化展示成客户已收到退款。
- 迁移 `035_fulfillment_sync_health.sql` 记录同步失败、退避时间；保留最后确认结果，单笔查询失败不阻塞其他订单，成功重试清除提示。
- 40 项充值和游客交付流程测试通过；独立 PostgreSQL 验证订单授权与跨订单同步失败隔离；桌面和手机界面预览通过，检测期间修改 Session 后充值按钮保持不可用。

这些验证使用隔离的虚构订单和供应商响应；没有为未付款客户开卡或扣款，没有开启生产邮箱或支付服务。
