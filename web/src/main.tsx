import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  Outlet,
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
} from '@tanstack/react-router'
import './index.css'
import { Layout } from './components/Layout'
import { DashboardPage } from './pages/Dashboard'
import { MachinesPage, MachineDetailPage } from './pages/Machines'
import { ActivityPage } from './pages/Activity'
import { AuditPage } from './pages/Audit'
import { RestorePage, SettingsPage } from './pages/Stubs'

const rootRoute = createRootRoute({
  component: () => (
    <Layout>
      <Outlet />
    </Layout>
  ),
})

const indexRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  component: DashboardPage,
})

const machinesRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/machines',
  component: MachinesPage,
})

const machineDetailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/machines/$id',
  component: MachineDetailPage,
})

const activityRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/activity',
  component: ActivityPage,
})

const restoreRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/restore',
  component: RestorePage,
})

const auditRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/audit',
  component: AuditPage,
})

const settingsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/settings',
  component: SettingsPage,
})

const routeTree = rootRoute.addChildren([
  indexRoute,
  machinesRoute,
  machineDetailRoute,
  activityRoute,
  restoreRoute,
  auditRoute,
  settingsRoute,
])

const router = createRouter({ routeTree })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: 1,
      staleTime: 5_000,
    },
  },
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
)
