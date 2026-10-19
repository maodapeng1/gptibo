import { PaymentLogo } from './payment-logo'
import { PaymentMethodChoices, type PaymentMethodOption } from './payment-method-choices'
import { paymentChannel } from '@/lib/payment-channel'
import { XAccount, XRechargeStatus } from './x-premium'
import { CopyButton } from './copy-button'
import { useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { toString as qrToString } from 'qrcode'
import { ExternalLink, RefreshCw } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { api, errorMessage } from '@/lib/api'
import { visiblePolling } from '@/lib/visible-polling'
import type { ServerOrder } from '@/lib/use-store'
import type { CheckoutProps } from './checkout'
import { PaymentConfirmed, PaymentMessage } from './payment-feedback'
import { membershipClaimUrl } from '../../shared/membership-claim'
import '@/styles/live-checkout.css'

type Payment = { amountCents?: number; feeCents?: number; feeBps?: number; provider?: string; status: string; qr: string | null; expiresAt: number }
export function ManualCheckout({ order, open, onUpdated, onOrderUpdated, onOrders, refreshTarget, refreshDisabled }: CheckoutProps & { order: ServerOrder }) {
  const [payment, setPayment] = useState<Payment | null>(null)
  const [methods, setMethods] = useState<PaymentMethodOption[]>([])
  const paymentRef = useRef<Payment | null>(null)
  const [qrImage, setQrImage] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const locked = useRef(false)
  const callbacks = useRef({ onUpdated, onOrderUpdated })
  useEffect(() => { callbacks.current = { onUpdated, onOrderUpdated } }, [onUpdated, onOrderUpdated])
  const channel = paymentChannel(payment?.provider)
  const xgift = order.xgift
  const gmail = order.plan === 'gmail-account'
  const claimUrl = gmail ? null : membershipClaimUrl(order.delivery?.value)
  const emailDelivery = Boolean(order.delivery)
  const automatic = Boolean(order.delivery?.automatic)
  const paused = order.redemptionBlocked || order.refundHold || (order.refundedCents ?? 0) >= Math.round(order.price * 100)
  const expired = payment !== null && payment.expiresAt <= Date.now()
  const qr = !paused && !expired && order.status === 'pending' && payment?.status === 'pending' ? payment.qr : null
  useEffect(() => {
    let active = true
    if (qr) void qrToString(qr, { type: 'svg', width: 156, margin: 4 })
      .then(svg => { if (active) setQrImage(`data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`) })
      .catch(() => { if (active) setError('二维码加载失败，请刷新进度。') })
    return () => { active = false }
  }, [qr])
  async function refresh(action: 'create' | 'query' | 'close' | 'options' = paymentRef.current ? 'query' : 'options', method?: string) {
    if (locked.current) return
    locked.current = true; setBusy(true)
    if (action === 'create') setError('')
    try {
      if (order.status === 'pending') {
        let result = await api<{ payment: Payment | null; methods?: PaymentMethodOption[] }>(`/api/orders/${order.id}/payment`, { action, method, expectedFeeBps: method ? methods.find(m => m.method === method)?.feeBps ?? 0 : undefined })
        setPayment(result.payment); paymentRef.current = result.payment
        if (result.methods) setMethods(result.methods)
        if ((action === 'options' || action === 'query') && result.payment?.status === 'pending' && !result.payment.qr && result.payment.expiresAt > Date.now()) {
          result = await api<{ payment: Payment }>(`/api/orders/${order.id}/payment`, { action: 'create' })
          setPayment(result.payment); paymentRef.current = result.payment
        }
      }
      const result = await api<{ order: ServerOrder }>(`/api/orders/${order.id}`)
      callbacks.current.onOrderUpdated?.(result.order)
      if (action !== 'query' || paymentRef.current?.qr || paymentRef.current?.status !== 'pending') setError('')
    } catch (e) { setError(errorMessage(e)) }
    finally { locked.current = false; setBusy(false); if (!callbacks.current.onOrderUpdated) callbacks.current.onUpdated?.() }
  }
  useEffect(() => {
    if (!open || paused || order.status === 'cancelled' || order.status === 'success') return
    return visiblePolling(() => refresh(), 5000)
    // Restart only when the order moves to a different stage.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, order.id, order.status, paused])
  const refreshButton = <Button variant="ghost" className="flow-refresh-action" disabled={busy || refreshDisabled} onClick={() => void refresh(payment ? 'query' : 'options')}><RefreshCw size={13} className={busy ? 'processing-spinner' : undefined} />{busy ? '正在核对…' : '刷新进度'}</Button>
  return <div className="checkout-body live-checkout" aria-busy={busy}>
    {xgift && <XAccount username={xgift.username} />}
    {emailDelivery && !xgift && order.purchaseNote && <details><summary>本订单购买说明</summary><p style={{ whiteSpace: 'pre-line', fontSize: 13 }}>{order.purchaseNote}</p></details>}
    {error && <p role="alert" className="copy-error">{error}</p>}
    {paused ? <div className="order-service-paused">本订单暂时暂停处理，请联系支持或查看售后进度。</div> : <>
      {order.status === 'pending' && <>
        {!payment ? <PaymentMethodChoices baseCents={Math.round(order.price * 100)} methods={methods} busy={busy} onChoose={method => void refresh('create', method)} /> : qr ? <div className="live-pay-card"><div className="live-pay-header"><div className="live-pay-brand"><PaymentLogo method={payment?.provider?.endsWith('-wxpay') ? 'wxpay' : 'alipay'} /><strong>{channel.name}</strong></div></div><div className="live-pay-body"><div className="live-qr-frame">{qrImage && <img src={qrImage} width={156} height={156} alt={`${channel.name}付款二维码`} />}</div><div className="live-pay-details"><div className="live-pay-amount"><span className="live-pay-amount-label">应付金额</span><strong className="live-pay-amount-value"><small>¥</small>{((payment?.amountCents ?? order.paymentSummary?.amountCents ?? Math.round(order.price * 100)) / 100).toFixed(2)}</strong></div>{Boolean(payment?.feeCents) && <p className="live-pay-tip">商品 ¥{order.price.toFixed(2)} + 平台手续费 ¥{((payment?.feeCents ?? 0) / 100).toFixed(2)}（{(payment?.feeBps ?? 0) / 100}%）</p>}<p className="live-pay-tip">{order.commerceDelivery ? '付款确认后，继续检测账号并办理充值。' : xgift ? '扫码付款，确认后自动开始充值。' : gmail ? '付款确认后，账号资料自动发送至下单邮箱。' : automatic ? '付款确认后自动发货，订单页和邮箱均可领取。' : emailDelivery ? '付款后由工作人员将商品发至下单邮箱。' : '付款后由专人协助开通 Pro 25×。'}</p><Button nativeButton={false} className="live-mobile-action" render={<a href={qr} target="_blank" rel="noopener noreferrer" />}>{channel.action} <ExternalLink size={13} /></Button></div></div>{xgift && <div className="live-pay-footer"><span className="live-watch"><i aria-hidden="true" />等待付款，状态自动更新</span></div>}</div>
          : <PaymentMessage state={expired ? 'expired' : error ? 'idle' : 'loading'} title={expired ? '付款有效期已结束' : error ? '付款入口暂未准备好' : '正在准备付款二维码'}>{!expired && error && !busy && payment?.status === 'pending' && <Button variant="outline" disabled={busy} onClick={() => void refresh('create')}>重新获取付款码</Button>}{expired ? '如已付款，请刷新核对结果，勿重复支付。' : !error ? '请稍候，付款二维码准备好后会自动显示。' : xgift ? '付款确认后将自动开始充值。' : emailDelivery ? '可刷新进度重试；付款后请等待邮件发货。' : '可刷新进度重试；付款后由专人协助开通。'}</PaymentMessage>}
      </>}
      {xgift && payment && order.status === 'pending' && <Button variant="ghost" disabled={busy} onClick={() => void refresh('close')}>关闭未付款订单</Button>}
      {!xgift && order.paymentStatus === 'paid' && <PaymentConfirmed>付款已确认，无需重复支付。</PaymentConfirmed>}
      {!xgift && !order.delivery?.value && ['session', 'processing'].includes(order.status) && <PaymentMessage state="idle" title={emailDelivery ? order.status === 'processing' ? '发货处理中' : '等待邮件发货' : order.status === 'processing' ? '正在人工开通' : '等待人工开通'}>{emailDelivery ? <>付款已确认，{automatic ? '系统将自动交付领取资料至订单页，并同步发送至' : gmail ? '系统将自动发送账号资料及使用说明至' : <>工作人员将把{order.delivery?.kind === 'link' ? '领取链接' : '兑换 CDK'}与说明发送至</>} <strong>{order.customerEmail}</strong>。请留意收件箱与垃圾邮件。</> : '请通过下方支持入口联系工作人员，提供订单号及要开通的账号。处理结果会更新到本订单。'}</PaymentMessage>}
      {order.delivery?.value && <section className="membership-direct-delivery" aria-label="领取资料">
        <h3>商品已发货</h3>
        <p>{claimUrl ? '专属领取链接' : '兑换 CDK'}</p>
        <pre style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere', userSelect: 'text' }}>{order.delivery.value}</pre>
        <div className="membership-delivery-actions">
          {claimUrl && <Button size="sm" nativeButton={false} render={<a href={claimUrl} target="_blank" rel="noopener noreferrer" />}>立即领取 <ExternalLink size={13} /></Button>}
          <CopyButton variant="outline" value={order.delivery.value} label={claimUrl ? '复制领取链接' : '复制 CDK'} />
        </div>
        <p style={{ whiteSpace: 'pre-line' }}>{order.delivery.instructions}</p>
        <p>{order.delivery.status === 'sent' ? '同一份资料已发送至' : '同一份资料正在同步发送至'} <strong>{order.customerEmail}</strong>。请妥善保管，勿转发他人。</p>
      </section>}
      {!xgift && !order.delivery?.value && order.status === 'success' && <PaymentMessage state="success" title={emailDelivery ? '商品已邮件发货' : '人工开通已完成'}>{emailDelivery ? <>发货邮件已提交邮件服务。请在 <strong>{order.customerEmail}</strong> 查收，并按邮件说明领取。若未收到，请联系支持核对。</> : '请前往 ChatGPT 查看订阅。'}</PaymentMessage>}
      {xgift && (order.status === 'session' || order.status === 'processing' || order.status === 'success' || order.status === 'failed') && <XRechargeStatus status={order.status} state={xgift.state} />}
      {order.status === 'cancelled' && <PaymentMessage state="closed" title="订单已关闭">如已付款，请联系支持核对本订单。</PaymentMessage>}
      {!xgift && order.status === 'failed' && <PaymentMessage state="idle" title="发货待处理">本次商品未完成交付，请通过原订单联系支持处理售后，无需重新付款。</PaymentMessage>}
      {order.status !== 'pending' && <Button className="primary-action" onClick={onOrders}>查看我的订单</Button>}
    </>}
    {refreshTarget ? createPortal(refreshButton, refreshTarget) : refreshButton}
  </div>
}
