import { isXGiftPlan } from '../../shared/xgift-products'
import { AsyncButton, PaymentConfirmed, RequestNotice } from './payment-feedback'
import { useState } from 'react'
import { ArrowRight, Check, ChevronLeft, CreditCard, FileCheck2, Info, LoaderCircle, LockKeyhole, RotateCcw, TriangleAlert, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { SuccessMark } from './brand'
import { SmsCheckout } from './sms-checkout'
import { DEMO_SESSION, ORDER_PLANS, type OrderAction } from '@/lib/demo-orders'

import { OrderRecord } from './order-record'
import { LiveCheckout } from './live-checkout'
import { ManualCheckout } from './manual-checkout'
import type { ServerOrder } from '@/lib/use-store'

export type CheckoutProps = { testingEnabled?: boolean; order: ServerOrder | undefined; open: boolean; onOpenChange: (open: boolean) => void; onAction: (id: string, action: OrderAction) => void; onOrders: () => void; busy?: boolean; pendingAction?: OrderAction; error?: string; onUpdated?: () => void; onOrderUpdated?: (order: ServerOrder) => void; refreshTarget?: HTMLElement | null; refreshDisabled?: boolean }
export function Checkout({ order, open, onOpenChange, onAction, onOrders, busy, pendingAction, error, onUpdated, testingEnabled, onOrderUpdated, refreshTarget, refreshDisabled }: CheckoutProps) {
  const [credential, setCredential] = useState('')
  const [outcome, setOutcome] = useState<'success' | 'failed'>('success')
  if(order?.mode==='live'&&order.commerceDelivery){if(order.status==='pending')return <ManualCheckout {...{order,open,onUpdated,onOrderUpdated,onOrders,refreshTarget,refreshDisabled,onOpenChange,onAction}}/>;return <OrderRecord {...{order,open,onOrders,onOpenChange,onAction,error}}/>}
  if (order?.mode === 'live' && (order.manualFulfillment || isXGiftPlan(order.plan))) return <ManualCheckout key={order.id} order={order} open={open} onUpdated={onUpdated} onOrderUpdated={onOrderUpdated} onOrders={onOrders} onOpenChange={onOpenChange} onAction={onAction} refreshTarget={refreshTarget} refreshDisabled={refreshDisabled} />
  if (order?.mode === 'live' && order.fulfillment) return <LiveCheckout key={order.id} order={order} open={open} onUpdated={onUpdated} onOrderUpdated={onOrderUpdated} onOrders={onOrders} error={error} refreshTarget={refreshTarget} refreshDisabled={refreshDisabled} />
  if (order && (order.mode !== 'demo' || !testingEnabled)) return <OrderRecord order={order} open={open} onOpenChange={onOpenChange} onAction={onAction} onOrders={onOrders} error={error} />
  if (order?.plan === 'codex-sms-once') return <SmsCheckout key={order.id} order={order} open={open} onOpenChange={onOpenChange} onAction={onAction} onOrders={onOrders} busy={busy} pendingAction={pendingAction} error={error} onUpdated={onUpdated} />
  if (!order) return null
  if (order.redemptionBlocked) return <div className="order-service-paused">本订单已由客服处理，无法再次兑换。<br />如有疑问，请联系支持。</div>
  const plan = ORDER_PLANS.find(p => p.id === order.plan)!
  const action = (event: OrderAction) => { if (!busy) onAction(order.id, event) }
  return <>
      {plan.renewalOnly && <p className="renewal-notice"><Info size={15} />仅支持已有订阅续费，暂不支持新开。</p>}

      {error && <p role="alert" className="copy-error">{error}</p>}
      {order.refundHold || (order.refundedCents ?? 0) >= Math.round(order.price * 100) ? <div className="order-service-paused">{order.refundHold ? '售后申请正在处理中，服务暂时暂停。' : '本订单已全额退款。'}<br />可以在下方查看售后进度。</div> : <fieldset disabled={busy} aria-busy={busy} className="checkout-body" style={{ minWidth: 0 }}>
        {order.status === 'pending' && <>
          <div className="payment-method"><div className="alipay-mark">支</div><div><strong>支付宝</strong><p>付款流程联调</p></div><Check size={17} /></div>
          <div className="payment-preview"><CreditCard size={28} strokeWidth={1.3} /><p>支付流程测试</p><span>不发起收款，可继续检查后续步骤</span></div>
          <div className="total-row"><span>本次合计</span><strong>¥{order.price.toFixed(2)}</strong></div>
          <AsyncButton className="primary-action" loading={pendingAction === 'pay'} pendingLabel="正在校验付款步骤" onClick={() => action('pay')}>跳过付款，继续测试 <ArrowRight /></AsyncButton>
          <button className="text-button quiet centered" onClick={() => action('cancel')}>{pendingAction === 'cancel' ? '正在关闭订单…' : '暂不购买，关闭此订单'}</button>
        </>}
        {order.status === 'session' && <>
          <PaymentConfirmed>付款步骤已通过，可继续检查账号连接。</PaymentConfirmed>
          <div className="field-group"><label htmlFor="demo-session">ChatGPT Session <LockKeyhole size={14} /></label><textarea id="demo-session" value={credential} readOnly placeholder="点击下方按钮，填入测试凭据" rows={4} /><p>使用固定测试凭据，请勿输入真实登录信息。</p></div>
          <Button variant="outline" className="fill-demo" onClick={() => setCredential(DEMO_SESSION)}><FileCheck2 size={16} />{credential ? '测试凭据已填入' : '填入测试凭据'}{credential && <Check size={15} />}</Button>
          <div className="demo-outcome"><label htmlFor="outcome">测试结果</label><select id="outcome" value={outcome} onChange={e => setOutcome(e.target.value as 'success' | 'failed')}><option value="success">充值成功</option><option value="failed">凭据失效</option></select></div>
          <AsyncButton className="primary-action" loading={pendingAction?.startsWith('submit-')} pendingLabel="正在提交测试凭据" disabled={!credential} onClick={() => action(outcome === 'success' ? 'submit-success' : 'submit-failed')}>提交并继续 <ArrowRight /></AsyncButton>
          <p className="micro-note"><LockKeyhole size={12} /> 测试凭据不会发送给服务商</p>
        </>}
        {order.status === 'processing' && <div className="result-body" role="status"><div className="result-icon"><LoaderCircle className="processing-spinner" size={34} strokeWidth={1.5} /></div><h3>正在查询结果</h3><p>正在检查提交与结果回调。<br />你可以关闭窗口，稍后在订单里查看。</p><div className="processing-track"><span /></div><span className="muted-small">测试结果约 5 秒返回</span></div>}
        {order.status === 'success' && <div className="result-body"><SuccessMark /><h3>本次测试已完成</h3><p>ChatGPT {plan.name} 流程测试已完成。<br />账号订阅未发生变化。</p><div className="result-receipt"><span>订单状态</span><span><Check size={14} /> 测试完成</span></div><Button className="primary-action" onClick={onOrders}>查看我的订单 <ArrowRight /></Button></div>}
        {order.status === 'failed' && <div className="result-body"><div className="result-icon warning"><TriangleAlert size={30} /></div><h3>再试一次，就从这里继续。</h3><p>当前测试场景为「Session 失效」。<br />选项与凭据已保留，调整后即可重试。</p><AsyncButton className="primary-action" loading={pendingAction === 'retry'} pendingLabel="正在恢复订单" onClick={() => action('retry')}><RotateCcw size={16} />重新提交凭据</AsyncButton></div>}
        {order.status === 'cancelled' && <div className="result-body"><div className="result-icon"><X size={30} /></div><h3>这次先到这里。</h3><p>订单已关闭，没有产生任何扣款。<br />随时回来，选择适合你的订阅。</p><Button variant="outline" className="primary-action" onClick={() => onOpenChange(false)}><ChevronLeft size={16} />返回订阅</Button></div>}
      </fieldset>}
      <div className="checkout-action-feedback">{busy && <RequestNotice key={pendingAction} label={pendingAction === 'pay' ? '正在校验付款步骤，请勿重复提交。' : pendingAction === 'cancel' ? '正在确认订单关闭结果。' : '正在保存操作，完成后自动继续。'} />}</div>
    </>
}
