<template>
  <div class="border-t border-gray-200 pt-4 dark:border-dark-600">
    <div class="flex items-center justify-between gap-4">
      <div>
        <label class="input-label mb-0">{{ t('admin.accounts.openai.requestTimezone') }}</label>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.accounts.openai.requestTimezoneDesc') }}
        </p>
      </div>
      <div class="w-64 flex-shrink-0">
        <Select
          :model-value="modelValue"
          :options="options"
          :disabled="loading || loadFailed"
          :loading="loading"
          searchable
          :aria-label="t('admin.accounts.openai.requestTimezone')"
          @update:model-value="selectTimezone"
        />
      </div>
    </div>
    <p v-if="loadFailed" class="mt-1 text-xs text-red-500">
      {{ t('admin.accounts.openai.requestTimezoneLoadFailed') }}
      <button type="button" class="ml-2 underline" @click="loadTimezones">
        {{ t('admin.accounts.openai.requestTimezoneRetry') }}
      </button>
    </p>
    <p v-else-if="unsupportedTimezone" class="mt-1 text-xs text-red-500">
      {{ t('admin.accounts.openai.requestTimezoneInvalid') }}
    </p>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import Select from '@/components/common/Select.vue'

const props = defineProps<{ modelValue: string }>()
const emit = defineEmits<{ (event: 'update:modelValue', value: string): void }>()
const { t } = useI18n()
const timezones = ref<string[]>([])
const loading = ref(false)
const loadFailed = ref(false)
const loaded = ref(false)
const unsupportedTimezone = computed(() => loaded.value && !timezones.value.includes(props.modelValue))
const options = computed(() => {
  const choices = timezones.value.map(value => ({ value, label: value, disabled: false }))
  // Keep the stored selection visible, even while loading or when it is invalid.
  // Never silently replace an account's setting after a failed list request.
  if (!timezones.value.includes(props.modelValue)) {
    choices.unshift({ value: props.modelValue, label: props.modelValue, disabled: true })
  }
  return choices
})

const selectTimezone = (value: string | number | boolean | null) => {
  if (typeof value === 'string' && timezones.value.includes(value)) {
    emit('update:modelValue', value)
  }
}

const loadTimezones = async () => {
  loading.value = true
  loadFailed.value = false
  try {
    const result = await adminAPI.accounts.getOpenAIRequestTimezones()
    if (!Array.isArray(result.timezones) || result.timezones.length === 0 ||
        result.timezones.some(value => typeof value !== 'string') ||
        !result.timezones.includes(result.default)) {
      throw new Error('Invalid request timezone list')
    }
    timezones.value = result.timezones
    loaded.value = true
  } catch {
    loadFailed.value = true
  } finally {
    loading.value = false
  }
}

onMounted(loadTimezones)
</script>
