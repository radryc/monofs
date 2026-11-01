<script setup lang="ts">
import { computed, type Component } from 'vue'
import {
  ChartColumn,
  CircleCheck,
  CircleX,
  Clock,
  Database,
  Download,
  Eye,
  FileText,
  Folder,
  FolderTree,
  Globe,
  HardDrive,
  Hourglass,
  Library,
  Monitor,
  Network,
  Package,
  RefreshCw,
  Rocket,
  Search,
  Server,
  Settings,
  ShieldCheck,
  Timer,
  TrendingUp,
  TriangleAlert,
  Wrench,
  Zap,
} from 'lucide-vue-next'

const props = defineProps<{
  icon?: Component | string
  label: string
  value: string | number
  sub?: string
  color?: 'violet' | 'emerald' | 'rose' | 'amber' | 'sky' | 'teal'
}>()

// Older pages still pass emoji names. Resolve them to the shared icon family
// here so the operator UI never renders emoji.
const LEGACY_ICONS: Record<string, Component> = {
  '⏱️': Timer,
  '⏳': Hourglass,
  '⚙️': Settings,
  '⚠️': TriangleAlert,
  '⚡': Zap,
  '✅': CircleCheck,
  '❌': CircleX,
  '🌍': Globe,
  '🌐': Network,
  '👁️': Eye,
  '💻': Monitor,
  '💾': HardDrive,
  '📁': Folder,
  '📄': FileText,
  '📈': TrendingUp,
  '📊': ChartColumn,
  '📚': Library,
  '📥': Download,
  '📦': Package,
  '🔄': RefreshCw,
  '🔍': Search,
  '🕒': Clock,
  '🖥️': Server,
  '🗂️': FolderTree,
  '🗃️': Database,
  '🚀': Rocket,
  '🛡️': ShieldCheck,
  '🧰': Wrench,
}

const resolvedIcon = computed(() =>
  typeof props.icon === 'string' ? LEGACY_ICONS[props.icon] : props.icon
)
</script>

<template>
  <div class="bg-slate-900/60 backdrop-blur-sm border border-slate-700/40 rounded-lg px-3 py-2.5 flex flex-wrap items-center gap-x-2 gap-y-1">
    <div class="flex items-center gap-1.5 min-w-0">
      <component v-if="resolvedIcon" :is="resolvedIcon" class="w-4 h-4 shrink-0 text-slate-400" aria-hidden="true" />
      <div class="text-xs text-slate-400 truncate">{{ label }}</div>
    </div>
    <div class="text-lg font-bold text-slate-100 leading-none shrink-0">{{ value }}</div>
    <div v-if="sub" class="basis-full text-xs text-slate-500 truncate">{{ sub }}</div>
  </div>
</template>
