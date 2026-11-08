export interface Order {
  id: number
  total: number
  status: string
}

export interface Customer {
  id: number
  name: string
  email: string
  status: string
  plan: string
  seats: number
  signup_date: string
  deleted_at: string | null
  last_login: string | null
  mrr: number
  Orders: Order[]
}

function customer(
  id: number, name: string, status: string, plan: string, seats: number, mrr: number,
  signup_date = '2024-06-15', overrides: Partial<Customer> = {},
): Customer {
  return {
    id, name, status, plan, seats, mrr, signup_date,
    email: `${name.toLowerCase().replaceAll(' ', '.')}@example.com`,
    deleted_at: null, last_login: '2026-10-20',
    Orders: [
      { id: id * 10 + 1, total: 75, status: 'paid' },
      { id: id * 10 + 2, total: 240, status: 'paid' },
      { id: id * 10 + 3, total: 160, status: 'refunded' },
    ],
    ...overrides,
  }
}

// Fixed dates and deliberately varied values keep every example reproducible.
export const CUSTOMERS: Customer[] = [
  customer(1, 'Alice Chen', 'active', 'pro', 12, 149),
  customer(2, 'Ben Carter', 'trialing', 'pro', 8, 299),
  customer(3, 'Carla Diaz', 'paused', 'enterprise', 80, 1299),
  customer(4, 'Daniel Evans', 'active', 'enterprise', 100, 2499, '2024-03-10', {
    Orders: [{ id: 41, total: 1000, status: 'paid' }, { id: 42, total: 600, status: 'paid' }, { id: 43, total: 70, status: 'pending' }],
  }),
  customer(5, 'Ella Foster', 'active', 'pro', 10, 199, '2023-08-05'),
  customer(6, 'Farah Green', 'active', 'pro', 15, 349, '2025-02-12', {
    Orders: [{ id: 61, total: 350, status: 'paid' }, { id: 62, total: 120, status: 'pending' }, { id: 63, total: 160, status: 'refunded' }],
  }),
  customer(7, 'George Hall', 'active', 'pro', 20, 399, '2024-07-20', { deleted_at: '2026-09-01' }),
  customer(8, 'Hana Ito', 'active', 'pro', 5, 129, '2024-04-14', { last_login: null }),
  customer(9, 'Ivan Jones', 'active', 'pro', 4, 49),
  customer(10, 'QA Kelly', 'active', 'enterprise', 60, 899),
  customer(11, 'Liam Moore', 'trialing', 'pro', 5, 199, '2024-10-02', { email: 'liam@test.local' }),
  customer(12, 'Maya Nair', 'paused', 'enterprise', 30, 799),
  customer(13, 'Noah Owen', 'churned', 'pro', 10, 0, '2024-01-15', { last_login: null, Orders: [] }),
  customer(14, 'Olivia Park', 'trialing', 'starter', 3, 119, '2024-11-01', {
    Orders: [{ id: 141, total: 90, status: 'paid' }],
  }),
  customer(15, 'Pablo Reed', 'active', 'enterprise', 50, 1499, '2024-12-31'),
  customer(16, 'Salma Stone', 'active', 'pro', 8, 299, '2024-01-01'),
  customer(17, 'Theo Taylor', 'paused', 'starter', 2, 29),
  customer(18, 'Zara West', 'active', 'pro', 10, 299, '2024-09-09', {
    Orders: [{ id: 181, total: 60, status: 'paid' }, { id: 182, total: 160, status: 'refunded' }],
  }),
]

export const CUSTOMER_FIELDS = [
  'id', 'name', 'email', 'status', 'plan', 'seats', 'signup_date',
  'deleted_at', 'last_login', 'mrr',
] as const
export const ORDER_FIELDS = ['id', 'total', 'status'] as const
