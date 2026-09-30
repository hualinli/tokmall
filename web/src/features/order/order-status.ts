import type { components } from '../../api/types'

export type OrderStatus = components['schemas']['OrderDetail']['status']
export type RefundStatus = components['schemas']['RefundDetail']['status']

/**
 * 订单状态文案。
 *
 * 类型刻意写成 Record<OrderStatus, string>：后端状态机新增状态时（如 REFUND_REQUESTED /
 * REFUNDED）这里会直接编译失败，逼前端补齐文案——而不是悄悄渲染出一个空白角标。
 */
export const ORDER_STATUS_LABELS: Record<OrderStatus, string> = {
  PENDING_PAYMENT: '待支付',
  PAID: '待发货',
  FULFILLING: '待确认收货',
  COMPLETED: '已完成',
  CANCELED: '已取消',
  EXPIRED: '已超时',
  REFUND_REQUESTED: '退款中',
  REFUNDED: '已退款',
}

/** 取不到文案时原样展示后端状态码（比空白强，也方便排查） */
export function orderStatusLabel(status: OrderStatus): string {
  return ORDER_STATUS_LABELS[status] ?? status
}

export const REFUND_STATUS_LABELS: Record<RefundStatus, string> = {
  REQUESTED: '待商家处理',
  REJECTED: '商家已拒绝',
  SUCCEEDED: '已退款',
}

export function refundStatusLabel(status: RefundStatus): string {
  return REFUND_STATUS_LABELS[status] ?? status
}
