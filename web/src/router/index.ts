import { createRouter, createWebHistory } from 'vue-router'
import DashboardView from '../views/DashboardView.vue'
import LoginView from '../views/LoginView.vue'
import RecordingsView from '../views/RecordingsView.vue'
import SecurityView from '../views/SecurityView.vue'
import AdminAccountsView from '../views/AdminAccountsView.vue'
import AdminOperationsView from '../views/AdminOperationsView.vue'
import { useSessionStore } from '../stores/session'

declare module 'vue-router' { interface RouteMeta { auth?: boolean; admin?: boolean } }

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', component: DashboardView },
    { path: '/login', component: LoginView },
    { path: '/recordings', component: RecordingsView, meta: { auth: true } },
    { path: '/security', component: SecurityView, meta: { auth: true } },
    { path: '/admin/accounts', component: AdminAccountsView, meta: { auth: true, admin: true } },
    { path: '/admin/operations', component: AdminOperationsView, meta: { auth: true, admin: true } },
  ],
  scrollBehavior: () => ({ top: 0 }),
})

router.beforeEach(async to => {
  const session = useSessionStore()
  if (!session.loaded) await session.refresh()
  if (to.meta.auth && !session.authenticated) return { path: '/login', query: { next: to.fullPath } }
  if (session.session.forced_password_change && to.path !== '/security') return '/security'
  if (to.meta.admin && !session.isAdmin) return '/'
})
