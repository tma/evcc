<template>
	<Card :title="$t('batterySettings.usageTab')" :subtitle="chargeSubtitle">
		<div class="d-flex gap-3 mb-4" data-testid="battery-priority">
			<shopicon-regular-sun
				size="s"
				class="text-primary flex-shrink-0 mt-1"
			></shopicon-regular-sun>
			<div>
				<div class="fw-bold mb-1">{{ $t("battery.config.priorityTitle") }}</div>
				<i18n-t
					:keypath="
						selectedPrioritySoc > 0
							? 'battery.config.priority'
							: 'battery.config.priorityNone'
					"
					tag="p"
					class="mb-0"
					scope="global"
				>
					<template #soc>
						<InlineSocSelect
							id="batteryExpPriority"
							:options="priorityOptions"
							:selected="selectedPrioritySoc"
							:label="fmtSoc(selectedPrioritySoc)"
							nowrap
							@change="changePrioritySoc"
						/>
					</template>
				</i18n-t>
			</div>
		</div>

		<div class="d-flex gap-3 mb-4" data-testid="battery-reserve">
			<shopicon-regular-lightning
				size="s"
				class="text-primary flex-shrink-0 mt-1"
			></shopicon-regular-lightning>
			<div>
				<div class="fw-bold mb-1">{{ $t("batterySettings.reserve.title") }}</div>
				<i18n-t
					keypath="batterySettings.reserve.description"
					tag="p"
					class="mb-0"
					scope="global"
				>
					<template #soc>
						<InlineSocSelect
							id="batteryExpReserve"
							:options="reserveOptions"
							:selected="selectedReserveSoc"
							:label="fmtSoc(selectedReserveSoc)"
							nowrap
							@change="changeReserveSoc"
						/>
					</template>
				</i18n-t>
				<i18n-t
					keypath="batterySettings.solarSupport.description"
					tag="p"
					class="mb-0"
					scope="global"
				>
					<template #mode>
						<CustomSelect
							id="batteryExpSolarSupport"
							:aria-label="$t('batterySettings.solarSupport.title')"
							:options="solarSupportOptions"
							:selected="String(batterySolarSupport)"
							inline
							@change="changeBatterySolarSupport"
						>
							<span class="text-decoration-underline fw-bold">
								{{ selectedSolarSupportName }}
							</span>
						</CustomSelect>
					</template>
				</i18n-t>
				<p v-if="batterySolarSupport" class="mb-0">
					{{ $t("batterySettings.legendTopAutostart") }}
					<InlineSocSelect
						id="batteryExpBufferStart"
						:aria-label="$t('batterySettings.legendTopAutostart')"
						:options="bufferStartOptions"
						:selected="selectedBufferStartSoc"
						:label="selectedBufferStartName"
						@change="changeBufferStart"
					/>
				</p>
				<p v-if="controllable" class="mb-0">
					<i18n-t
						keypath="batterySettings.dischargeMode.description"
						tag="span"
						scope="global"
					>
						<template #mode>
							<CustomSelect
								id="batteryExpDischargeMode"
								:aria-label="$t('batterySettings.dischargeMode.title')"
								:options="dischargeModeOptions"
								:selected="batteryDischargeMode"
								inline
								@change="changeDischargeMode"
							>
								<span class="text-decoration-underline fw-bold">
									{{ selectedDischargeModeName }}
								</span>
							</CustomSelect>
						</template> </i18n-t
					>.
				</p>
				<p v-if="showReserveUnavailableNote" class="mb-0">
					{{ $t("batterySettings.dischargeMode.reserveUnavailable") }}.
				</p>
			</div>
		</div>

		<template v-if="controllable && experimental">
			<div class="form-check form-switch mt-2">
				<input
					id="batteryExpGridDischarge"
					:checked="batteryGridDischarge"
					class="form-check-input"
					type="checkbox"
					role="switch"
					@change="changeGridDischarge"
				/>
				<label class="form-check-label" for="batteryExpGridDischarge">
					{{ $t("battery.config.gridDischarge") }} 🧪
				</label>
			</div>
		</template>
	</Card>
</template>

<script lang="ts">
import "@h2d2/shopicons/es/regular/sun";
import "@h2d2/shopicons/es/regular/lightning";
import { defineComponent, type PropType } from "vue";
import formatter from "@/mixins/formatter";
import api from "@/api";
import { BATTERY_DISCHARGE_MODE, type Battery } from "@/types/evcc";
import Card from "../Helper/Card.vue";
import CustomSelect from "../Helper/CustomSelect.vue";
import InlineSocSelect from "./InlineSocSelect.vue";

// Battery usage controls for the experimental page. The logic is intentionally duplicated
// from the classic BatteryUsageSettings.vue (slated for removal) so the two can diverge
// during the transition.
export default defineComponent({
	name: "BatteryConfigCard",
	components: { Card, CustomSelect, InlineSocSelect },
	mixins: [formatter],
	props: {
		batteryReserveSoc: { type: Number, default: 100 },
		batterySolarSupport: Boolean,
		prioritySoc: { type: Number, default: 0 },
		bufferStartSoc: { type: Number, default: 0 },
		batteryDischargeMode: {
			type: String as PropType<BATTERY_DISCHARGE_MODE>,
			default: BATTERY_DISCHARGE_MODE.ALLOW,
		},
		batteryGridDischarge: Boolean,
		battery: { type: Object as PropType<Battery> },
		experimental: Boolean,
	},
	data() {
		return {
			selectedReserveSoc: 100,
			selectedPrioritySoc: 0,
			selectedBufferStartSoc: 0,
		};
	},
	computed: {
		chargeSubtitle(): string {
			return `${this.$t("battery.card.soc")} ${this.fmtSoc(this.batterySoc)}`;
		},
		batterySoc(): number {
			return this.battery?.soc ?? 0;
		},
		controllable(): boolean {
			return (this.battery?.devices ?? []).some(({ controllable }) => controllable);
		},
		reserveDischargeAvailable(): boolean {
			return this.selectedReserveSoc > 0 && this.selectedReserveSoc < 100;
		},
		dischargeModeOptions() {
			return Object.values(BATTERY_DISCHARGE_MODE).map((value) => ({
				value,
				name: this.$t(`batterySettings.dischargeMode.${value}`),
				disabled:
					value === BATTERY_DISCHARGE_MODE.RESERVE && !this.reserveDischargeAvailable,
			}));
		},
		showReserveUnavailableNote(): boolean {
			return (
				this.batteryDischargeMode === BATTERY_DISCHARGE_MODE.RESERVE &&
				!this.reserveDischargeAvailable
			);
		},
		selectedDischargeModeName(): string {
			return (
				this.dischargeModeOptions.find(({ value }) => value === this.batteryDischargeMode)
					?.name ?? ""
			);
		},
		priorityOptions() {
			const options = [];
			for (let i = 100; i >= 0; i -= 5) {
				const disabled =
					this.batterySolarSupport &&
					(i >= 100 ||
						(i > this.selectedReserveSoc &&
							this.selectedReserveSoc !== this.selectedPrioritySoc));
				options.push({ value: i, name: this.fmtSoc(i), disabled });
			}
			return options;
		},
		reserveOptions() {
			const options = [];
			for (let i = 100; i >= 0; i -= 5) {
				options.push({
					value: i,
					name: this.fmtSoc(i),
					disabled:
						this.batterySolarSupport &&
						(i <= 0 || i >= 100 || i < this.selectedPrioritySoc),
				});
			}
			return options;
		},
		solarSupportOptions() {
			const canEnable =
				this.selectedReserveSoc > 0 &&
				this.selectedReserveSoc < 100 &&
				this.selectedReserveSoc >= this.selectedPrioritySoc &&
				(!this.selectedBufferStartSoc ||
					this.selectedReserveSoc <= this.selectedBufferStartSoc);
			return [false, true].map((value) => ({
				value: String(value),
				name: this.$t(`batterySettings.solarSupport.${value ? "enabled" : "disabled"}`),
				disabled: value && !canEnable,
			}));
		},
		selectedSolarSupportName(): string {
			return this.$t(
				`batterySettings.solarSupport.${this.batterySolarSupport ? "enabled" : "disabled"}`
			);
		},
		bufferStartOptions() {
			const options = [];
			for (let i = 100; i >= this.selectedReserveSoc; i -= 5) {
				options.push({ value: i, name: this.getBufferStartName(i) });
			}
			options.push({ value: 0, name: this.getBufferStartName(0) });
			return options;
		},
		selectedBufferStartName(): string {
			return this.getBufferStartName(this.selectedBufferStartSoc);
		},
	},
	watch: {
		prioritySoc: {
			handler(soc) {
				this.selectedPrioritySoc = soc;
			},
			immediate: true,
		},
		batteryReserveSoc: {
			handler(soc) {
				this.selectedReserveSoc = soc;
			},
			immediate: true,
		},
		bufferStartSoc: {
			handler(soc) {
				this.selectedBufferStartSoc = soc;
			},
			immediate: true,
		},
	},
	methods: {
		async changePrioritySoc($event: Event) {
			const soc = parseInt(($event.target as HTMLInputElement).value, 10);
			if (this.batterySolarSupport && soc > this.selectedReserveSoc) {
				if (soc > this.selectedBufferStartSoc && this.selectedBufferStartSoc > 0) {
					if (!(await this.setBufferStartSoc(soc))) {
						return;
					}
				}
				await this.saveReserveSoc(soc);
			} else {
				await this.savePrioritySoc(soc);
			}
		},
		changeBufferStart($event: Event) {
			this.setBufferStartSoc(parseInt(($event.target as HTMLInputElement).value, 10));
		},
		async changeReserveSoc($event: Event) {
			const soc = parseInt(($event.target as HTMLInputElement).value, 10);
			if (soc > this.selectedBufferStartSoc && this.selectedBufferStartSoc > 0) {
				if (!(await this.setBufferStartSoc(soc))) {
					return;
				}
			}
			await this.saveReserveSoc(soc);
		},
		async setBufferStartSoc(soc: number) {
			this.selectedBufferStartSoc = soc;
			return this.saveBufferStartSoc(soc);
		},
		async savePrioritySoc(soc: number) {
			this.selectedPrioritySoc = soc;
			try {
				await api.post(`prioritysoc/${encodeURIComponent(soc)}`);
			} catch (err) {
				this.selectedPrioritySoc = this.prioritySoc;
				console.error(err);
			}
		},
		async saveReserveSoc(soc: number) {
			this.selectedReserveSoc = soc;
			try {
				await api.post(`batteryreservesoc/${encodeURIComponent(soc)}`);
			} catch (err) {
				this.selectedReserveSoc = this.batteryReserveSoc;
				console.error(err);
			}
		},
		async changeBatterySolarSupport(e: Event) {
			const target = e.target as HTMLSelectElement;
			const enabled = target.value === "true";
			try {
				await api.post(`batterysolarsupport/${enabled}`);
			} catch (err) {
				target.value = String(this.batterySolarSupport);
				console.error(err);
			}
		},
		async saveBufferStartSoc(soc: number) {
			try {
				await api.post(`bufferstartsoc/${encodeURIComponent(soc)}`);
				return true;
			} catch (err) {
				this.selectedBufferStartSoc = this.bufferStartSoc;
				console.error(err);
				return false;
			}
		},
		async changeDischargeMode(e: Event) {
			const target = e.target as HTMLSelectElement;
			try {
				await api.post(`batterydischargemode/${encodeURIComponent(target.value)}`);
			} catch (err) {
				target.value = this.batteryDischargeMode;
				console.error(err);
			}
		},
		async changeGridDischarge(e: Event) {
			const target = e.target as HTMLInputElement;
			try {
				await api.post(`batterygriddischarge/${target.checked}`);
			} catch (err) {
				target.checked = this.batteryGridDischarge; // revert to stay in sync with state
				console.error(err);
			}
		},
		getBufferStartName(value: number) {
			const key = value === 0 ? "never" : value === 100 ? "full" : "above";
			return this.$t(`battery.config.bufferStart.${key}`, { soc: this.fmtSoc(value) });
		},
		fmtSoc(soc: number) {
			return this.fmtPercentage(soc);
		},
	},
});
</script>
