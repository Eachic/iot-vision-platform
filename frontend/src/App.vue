<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import axios from 'axios'

const api = axios.create({
  baseURL: 'http://127.0.0.1:8080/api',
  timeout: 8000
})

const AUTH_TOKEN_KEY = 'iot_vision_token'

const STATUS_LABELS = {
  queued: '排队中',
  processing: '处理中',
  completed: '已完成',
  failed: '失败'
}

const activeView = ref('overview')
const loading = ref(false)
const authLoading = ref(false)
const bootstrapping = ref(true)
const apiError = ref('')
const authError = ref('')
const lastUpdated = ref('')
const preview = ref(null)
const authToken = ref(localStorage.getItem(AUTH_TOKEN_KEY) || '')
const currentUser = ref(null)
const loginForm = ref({ username: 'admin', password: 'admin123456' })
const images = ref([])
const devices = ref([])
const taskStatus = ref({ counts: {}, recent: [] })
const stats = ref({ images: 0, devices: 0, today: 0, tags: [] })
const filters = ref({ device_id: '', status: '', tag: '' })
const pagination = ref({ page: 1, page_size: 60, total: 0 })
const sortOptions = ref({ sort_by: 'created_at', sort_order: 'desc' })
let refreshTimer = 0

const navItems = [
  { id: 'overview', label: '总览', icon: '⌂' },
  { id: 'gallery', label: '图库', icon: '▦' },
  { id: 'devices', label: '设备', icon: '◎' },
  { id: 'tasks', label: '任务', icon: '↻' }
]

const pageTitle = computed(() => {
  const item = navItems.find(nav => nav.id === activeView.value)
  return item ? item.label : '总览'
})

const isAuthenticated = computed(() => Boolean(authToken.value && currentUser.value))

api.interceptors.request.use(config => {
  if (authToken.value) {
    config.headers = config.headers || {}
    config.headers.Authorization = `Bearer ${authToken.value}`
  }
  return config
})

api.interceptors.response.use(
  response => response,
  error => {
    if (error && error.response && error.response.status === 401) {
      clearAuth()
    }
    return Promise.reject(error)
  }
)

const availableTags = computed(() => {
  const set = new Set()
  images.value.forEach(image => {
    ;(image.tags || []).forEach(tag => set.add(tag.tag))
  })
  ;(stats.value.tags || []).forEach(tag => set.add(tag.tag))
  return Array.from(set)
})

const filteredImages = computed(() => images.value)

const totalPages = computed(() => {
  const total = Number(pagination.value.total || 0)
  const pageSize = Number(pagination.value.page_size || 1)
  return Math.max(1, Math.ceil(total / pageSize))
})

const galleryRange = computed(() => {
  const total = Number(pagination.value.total || 0)
  if (!total) return '0 / 0'
  const start = (pagination.value.page - 1) * pagination.value.page_size + 1
  const end = Math.min(total, pagination.value.page * pagination.value.page_size)
  return `${start}-${end} / ${total}`
})

const completedRatio = computed(() => {
  const counts = taskStatus.value.counts || {}
  const total = Object.values(counts).reduce((sum, value) => sum + Number(value || 0), 0)
  if (!total) return 0
  return Math.round((Number(counts.completed || 0) / total) * 100)
})

async function refresh() {
  if (!isAuthenticated.value) return
  loading.value = true
  try {
    const params = {}
    Object.entries(filters.value).forEach(([key, value]) => {
      if (value) params[key] = value
    })
    const [imageRes, deviceRes, taskRes, statRes] = await Promise.all([
      api.get('/images', {
        params: {
          ...params,
          page: pagination.value.page,
          page_size: pagination.value.page_size,
          sort_by: sortOptions.value.sort_by,
          sort_order: sortOptions.value.sort_order
        }
      }),
      api.get('/devices'),
      api.get('/tasks/status'),
      api.get('/stats')
    ])
    images.value = imageRes.data.items || []
    pagination.value.total = Number(imageRes.data.total || 0)
    pagination.value.page = Number(imageRes.data.page || pagination.value.page)
    pagination.value.page_size = Number(imageRes.data.page_size || pagination.value.page_size)
    devices.value = deviceRes.data.items || []
    taskStatus.value = taskRes.data || { counts: {}, recent: [] }
    stats.value = statRes.data || { images: 0, devices: 0, today: 0, tags: [] }
    apiError.value = ''
    lastUpdated.value = new Date().toLocaleTimeString()
  } catch (error) {
    apiError.value = error && error.message ? error.message : '无法连接后端 API'
  } finally {
    loading.value = false
  }
}

async function login() {
  authLoading.value = true
  authError.value = ''
  try {
    const res = await api.post('/auth/login', {
      username: loginForm.value.username,
      password: loginForm.value.password
    })
    authToken.value = res.data.token || ''
    currentUser.value = res.data.user || null
    if (!authToken.value || !currentUser.value) {
      throw new Error('登录响应缺少 token')
    }
    localStorage.setItem(AUTH_TOKEN_KEY, authToken.value)
    await refresh()
  } catch (error) {
    clearAuth()
    authError.value = error && error.response && error.response.data && error.response.data.error
      ? error.response.data.error
      : '登录失败，请检查账号密码'
  } finally {
    authLoading.value = false
  }
}

async function loadCurrentUser() {
  if (!authToken.value) {
    bootstrapping.value = false
    return
  }
  try {
    const res = await api.get('/auth/me')
    currentUser.value = res.data.user || null
    if (currentUser.value) {
      await refresh()
    } else {
      clearAuth()
    }
  } catch (error) {
    clearAuth()
  } finally {
    bootstrapping.value = false
  }
}

function logout() {
  clearAuth()
  apiError.value = ''
  authError.value = ''
  images.value = []
  devices.value = []
  taskStatus.value = { counts: {}, recent: [] }
  stats.value = { images: 0, devices: 0, today: 0, tags: [] }
}

function clearAuth() {
  authToken.value = ''
  currentUser.value = null
  localStorage.removeItem(AUTH_TOKEN_KEY)
}

function resetFilters() {
  filters.value = { device_id: '', status: '', tag: '' }
  pagination.value.page = 1
  refresh()
}

function applyGalleryQuery() {
  pagination.value.page = 1
  refresh()
}

function changePage(delta) {
  const next = Math.min(totalPages.value, Math.max(1, pagination.value.page + delta))
  if (next === pagination.value.page) return
  pagination.value.page = next
  refresh()
}

function imageUrl(image) {
  const path = image.thumbnail_url || image.original_url
  return apiAssetUrl(path)
}

function originalUrl(image) {
  return apiAssetUrl(image && image.original_url)
}

function apiAssetUrl(path) {
  if (!path) return ''
  if (/^https?:\/\//i.test(path)) return path
  if (/^\/\//.test(path)) return `https:${path}`
  if (/^[^/?#]+\.[^/?#]+/.test(path)) return `https://${path}`
  return path.startsWith('/') ? `http://127.0.0.1:8080${path}` : `http://127.0.0.1:8080/${path}`
}

function statusText(status) {
  return STATUS_LABELS[status] || status || '未知'
}

function formatTime(value) {
  if (!value) return '-'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString()
}

function fileSize(size) {
  const value = Number(size || 0)
  if (value < 1024) return `${value} B`
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KB`
  return `${(value / 1024 / 1024).toFixed(1)} MB`
}

onMounted(() => {
  loadCurrentUser()
  refreshTimer = window.setInterval(refresh, 5000)
})

onBeforeUnmount(() => {
  if (refreshTimer) window.clearInterval(refreshTimer)
})
</script>

<template>
  <div v-if="bootstrapping" class="auth-shell">
    <section class="auth-card">
      <div class="brand-logo">IoT</div>
      <h1>视觉数据平台</h1>
      <p>正在验证登录状态...</p>
    </section>
  </div>

  <div v-else-if="!isAuthenticated" class="auth-shell">
    <section class="auth-card login-card">
      <div class="brand-logo">IoT</div>
      <div>
        <p class="eyebrow">IoT Vision Data Platform</p>
        <h1>登录视觉数据平台</h1>
        <p>使用管理员账号进入图库、设备、任务和统计控制台。</p>
      </div>
      <form class="login-form" @submit.prevent="login">
        <label>
          <span>账号</span>
          <input v-model.trim="loginForm.username" autocomplete="username" placeholder="admin">
        </label>
        <label>
          <span>密码</span>
          <input v-model="loginForm.password" autocomplete="current-password" placeholder="admin123456" type="password">
        </label>
        <button class="primary-btn" :disabled="authLoading" type="submit">
          {{ authLoading ? '登录中...' : '登录' }}
        </button>
      </form>
      <div v-if="authError" class="notice auth-notice">{{ authError }}</div>
      <small>默认账号：admin / admin123456</small>
    </section>
  </div>

  <div v-else class="app-shell">
    <aside class="app-sidebar">
      <div class="brand-block">
        <div class="brand-logo">IoT</div>
        <div>
          <h1>视觉数据平台</h1>
          <p>云边端协同</p>
        </div>
      </div>

      <nav class="nav-list">
        <button
          v-for="item in navItems"
          :key="item.id"
          class="nav-item"
          :class="{ active: activeView === item.id }"
          @click="activeView = item.id"
        >
          <span class="nav-icon">{{ item.icon }}</span>
          <span>{{ item.label }}</span>
        </button>
      </nav>

      <div class="pipeline-card">
        <span>Device</span>
        <b></b>
        <span>Edge</span>
        <b></b>
        <span>Cloud</span>
        <b></b>
        <span>AI Worker</span>
      </div>
    </aside>

    <main class="app-main">
      <header class="hero">
        <div>
          <div class="eyebrow">IoT Vision Data Platform</div>
          <h2>{{ pageTitle }}</h2>
          <p>设备模拟采集、边缘去重缓存、云端入库、异步缩略图与智能标签生成。</p>
        </div>
        <div class="hero-actions">
          <span class="refresh-time">{{ currentUser.username }} · {{ currentUser.role }}</span>
          <span class="refresh-time">最近刷新 {{ lastUpdated || '-' }}</span>
          <button class="primary-btn" :disabled="loading" @click="refresh">
            {{ loading ? '刷新中...' : '刷新数据' }}
          </button>
          <button class="ghost-btn" @click="logout">退出登录</button>
        </div>
      </header>

      <section v-if="apiError" class="notice">
        <strong>后端 API 暂时不可用</strong>
        <span>{{ apiError }}。请确认 cloud-api.exe 正在运行并监听 8080。</span>
      </section>

      <section class="metric-grid">
        <article class="metric-card">
          <span>图片总数</span>
          <strong>{{ stats.images || 0 }}</strong>
          <small>云端 MySQL 元数据</small>
        </article>
        <article class="metric-card">
          <span>在线设备</span>
          <strong>{{ stats.devices || devices.length || 0 }}</strong>
          <small>端侧模拟摄像头</small>
        </article>
        <article class="metric-card">
          <span>今日上传</span>
          <strong>{{ stats.today || 0 }}</strong>
          <small>边缘节点转发</small>
        </article>
        <article class="metric-card accent">
          <span>完成率</span>
          <strong>{{ completedRatio }}%</strong>
          <small>Worker 处理状态</small>
        </article>
      </section>

      <section v-if="activeView === 'overview'" class="overview-grid">
        <article class="panel large-panel">
          <div class="panel-head">
            <div>
              <h3>最新视觉数据</h3>
              <p>最近进入平台的图片任务</p>
            </div>
            <button class="ghost-btn" @click="activeView = 'gallery'">查看图库</button>
          </div>
          <div v-if="filteredImages.length" class="latest-strip">
            <button v-for="image in filteredImages.slice(0, 5)" :key="image.image_id" class="latest-tile" @click="preview = image">
              <img v-if="imageUrl(image)" :src="imageUrl(image)" :alt="image.image_id">
              <span v-else>处理中</span>
            </button>
          </div>
          <div v-else class="empty-state">暂无图片，启动设备模拟器后会自动出现。</div>
        </article>

        <article class="panel">
          <div class="panel-head">
            <div>
              <h3>任务状态</h3>
              <p>流处理队列概览</p>
            </div>
          </div>
          <div class="status-stack">
            <div v-for="status in ['queued','processing','completed','failed']" :key="status" class="status-row">
              <span class="status-dot" :class="'is-' + status"></span>
              <span>{{ statusText(status) }}</span>
              <strong>{{ (taskStatus.counts && taskStatus.counts[status]) || 0 }}</strong>
            </div>
          </div>
        </article>

        <article class="panel">
          <div class="panel-head">
            <div>
              <h3>标签分布</h3>
              <p>轻量 AI 识别结果</p>
            </div>
          </div>
          <div v-if="stats.tags && stats.tags.length" class="tag-cloud">
            <span v-for="tag in stats.tags" :key="tag.tag">{{ tag.tag }} · {{ tag.count }}</span>
          </div>
          <div v-else class="empty-state compact">暂无标签</div>
        </article>
      </section>

      <section v-if="activeView === 'gallery'" class="panel">
        <div class="panel-head gallery-head">
          <div>
            <h3>图片图库</h3>
            <p>按设备、状态、标签、时间排序和分页浏览</p>
          </div>
          <div class="filters">
            <select v-model="filters.device_id" @change="applyGalleryQuery">
              <option value="">全部设备</option>
              <option v-for="device in devices" :key="device.device_id" :value="device.device_id">{{ device.device_id }}</option>
            </select>
            <select v-model="filters.status" @change="applyGalleryQuery">
              <option value="">全部状态</option>
              <option value="queued">排队中</option>
              <option value="processing">处理中</option>
              <option value="completed">已完成</option>
              <option value="failed">失败</option>
            </select>
            <select v-model="filters.tag" @change="applyGalleryQuery">
              <option value="">全部标签</option>
              <option v-for="tag in availableTags" :key="tag" :value="tag">{{ tag }}</option>
            </select>
            <select v-model="sortOptions.sort_by" @change="applyGalleryQuery">
              <option value="created_at">入库时间</option>
              <option value="captured_at">采集时间</option>
              <option value="updated_at">更新时间</option>
              <option value="size">文件大小</option>
              <option value="device_id">设备 ID</option>
            </select>
            <select v-model="sortOptions.sort_order" @change="applyGalleryQuery">
              <option value="desc">降序</option>
              <option value="asc">升序</option>
            </select>
            <select v-model.number="pagination.page_size" @change="applyGalleryQuery">
              <option :value="30">每页 30</option>
              <option :value="60">每页 60</option>
              <option :value="100">每页 100</option>
              <option :value="200">每页 200</option>
            </select>
            <button class="ghost-btn" @click="resetFilters">重置</button>
          </div>
        </div>

        <div v-if="filteredImages.length" class="gallery-grid">
          <article v-for="image in filteredImages" :key="image.image_id" class="image-card" @click="preview = image">
            <div class="image-frame">
              <img v-if="imageUrl(image)" :src="imageUrl(image)" :alt="image.image_id">
              <span v-else>处理中</span>
            </div>
            <div class="image-info">
              <div class="image-title">
                <strong>{{ image.device_id }}</strong>
                <span class="status-pill" :class="'is-' + image.status">{{ statusText(image.status) }}</span>
              </div>
              <p>{{ image.image_id }}</p>
              <div class="image-meta">
                <span>{{ image.width || '-' }} × {{ image.height || '-' }}</span>
                <span>{{ fileSize(image.size) }}</span>
              </div>
              <div class="tag-list">
                <span v-for="tag in image.tags || []" :key="tag.id || tag.tag">{{ tag.tag }}</span>
              </div>
            </div>
          </article>
        </div>
        <div v-else class="empty-state">暂无符合条件的图片。</div>

        <div class="pagination-bar">
          <div>
            <strong>{{ galleryRange }}</strong>
            <span>第 {{ pagination.page }} / {{ totalPages }} 页</span>
          </div>
          <div class="pagination-actions">
            <button class="ghost-btn" :disabled="pagination.page <= 1" @click="changePage(-1)">上一页</button>
            <button class="ghost-btn" :disabled="pagination.page >= totalPages" @click="changePage(1)">下一页</button>
          </div>
        </div>
      </section>

      <section v-if="activeView === 'devices'" class="panel">
        <div class="panel-head">
          <div>
            <h3>设备状态</h3>
            <p>模拟摄像头和边缘接入情况</p>
          </div>
        </div>
        <div class="data-table">
          <div class="table-row table-head">
            <span>设备 ID</span>
            <span>位置</span>
            <span>状态</span>
            <span>图片数</span>
            <span>最后上报</span>
          </div>
          <div v-for="device in devices" :key="device.device_id" class="table-row">
            <strong>{{ device.device_id }}</strong>
            <span>{{ device.location || '-' }}</span>
            <span class="status-pill is-completed">{{ device.status || 'online' }}</span>
            <span>{{ device.image_count || 0 }}</span>
            <span>{{ formatTime(device.last_seen) }}</span>
          </div>
        </div>
        <div v-if="!devices.length" class="empty-state">暂无设备，启动模拟器后会自动注册。</div>
      </section>

      <section v-if="activeView === 'tasks'" class="panel">
        <div class="panel-head">
          <div>
            <h3>流处理任务</h3>
            <p>queued → processing → completed</p>
          </div>
        </div>
        <div class="data-table">
          <div class="table-row table-head">
            <span>图片 ID</span>
            <span>设备</span>
            <span>状态</span>
            <span>更新时间</span>
            <span>错误信息</span>
          </div>
          <div v-for="task in taskStatus.recent || []" :key="task.image_id" class="table-row">
            <strong>{{ task.image_id }}</strong>
            <span>{{ task.device_id }}</span>
            <span class="status-pill" :class="'is-' + task.status">{{ statusText(task.status) }}</span>
            <span>{{ formatTime(task.updated_at) }}</span>
            <span>{{ task.error_message || '-' }}</span>
          </div>
        </div>
        <div v-if="!taskStatus.recent || !taskStatus.recent.length" class="empty-state">暂无任务记录。</div>
      </section>
    </main>

    <div v-if="preview" class="modal-backdrop" @click.self="preview = null">
      <article class="preview-modal">
        <button class="close-btn" @click="preview = null">×</button>
        <img v-if="originalUrl(preview)" :src="originalUrl(preview)" :alt="preview.image_id">
        <div class="preview-detail">
          <h3>{{ preview.image_id }}</h3>
          <p>{{ preview.device_id }} · {{ preview.edge_node_id }}</p>
          <div class="tag-list">
            <span v-for="tag in preview.tags || []" :key="tag.id || tag.tag">{{ tag.tag }} {{ Math.round((tag.confidence || 0) * 100) }}%</span>
          </div>
          <dl>
            <dt>尺寸</dt><dd>{{ preview.width }} × {{ preview.height }}</dd>
            <dt>大小</dt><dd>{{ fileSize(preview.size) }}</dd>
            <dt>格式</dt><dd>{{ preview.format }}</dd>
            <dt>采集时间</dt><dd>{{ formatTime(preview.captured_at) }}</dd>
          </dl>
        </div>
      </article>
    </div>
  </div>
</template>
