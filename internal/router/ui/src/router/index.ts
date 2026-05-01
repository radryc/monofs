import { createRouter, createWebHistory } from 'vue-router'

const routes = [
  { path: '/', component: () => import('../pages/DashboardPage.vue'), meta: { title: 'Dashboard' } },
  { path: '/cluster', component: () => import('../pages/ClusterPage.vue'), meta: { title: 'MetaStores' } },
  { path: '/clients', component: () => import('../pages/ClientsPage.vue'), meta: { title: 'Clients' } },
  { path: '/performance', component: () => import('../pages/PerformancePage.vue'), meta: { title: 'Performance' } },
  { path: '/replication', component: () => import('../pages/ReplicationPage.vue'), meta: { title: 'Replication' } },
  { path: '/repositories', component: () => import('../pages/RepositoriesPage.vue'), meta: { title: 'Repositories' } },
  { path: '/ingest', component: () => import('../pages/IngestPage.vue'), meta: { title: 'Ingest' } },
  { path: '/search', component: () => import('../pages/SearchPage.vue'), meta: { title: 'Search' } },
  { path: '/indexer', component: () => import('../pages/IndexerPage.vue'), meta: { title: 'Indexer' } },
  { path: '/fetchers', component: () => import('../pages/FetchersPage.vue'), meta: { title: 'Fetchers' } },
  { path: '/dependencies', component: () => import('../pages/DependenciesPage.vue'), meta: { title: 'Dependencies' } },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
})

router.afterEach((to) => {
  document.title = `${to.meta.title || 'MonoFS'} – MonoFS`
})
