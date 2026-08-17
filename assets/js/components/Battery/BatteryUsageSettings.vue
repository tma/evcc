<template>
	<div class="row">
		<p class="text-center text-md-start col-md-6 order-md-2 col-lg-3 order-lg-3 pt-lg-2">
			{{ $t("batterySettings.batteryLevel") }}:
			<strong>{{ fmtSoc(batterySoc) }}</strong>
			<small v-for="(line, index) in batteryDetails" :key="index" class="d-block">
				{{ line }}
			</small>
		</p>
		<div
			class="col-md-6 order-md-1 col-lg-3 order-lg-2 mb-5 mb-lg-0 battery justify-content-center justify-content-md-start"
		>
			<div class="batteryLimits">
				<CustomSelect
					id="batterySettingsBuffer"
					:options="reserveOptions"
					:selected="selectedReserveSoc"
					class="bufferSoc p-2 end-0"
					:class="{
						'bufferSoc--hidden':
							!batterySolarSupport || selectedReserveSoc === selectedPrioritySoc,
					}"
					:style="{ top: `${topHeight}%` }"
					@change="changeReserveSoc"
				>
					<span class="text-decoration-underline text-nowrap pe-none">
						{{ fmtSoc(selectedReserveSoc) }}
					</span>
				</CustomSelect>

				<CustomSelect
					id="batterySettingsPriority"
					:options="priorityOptions"
					:selected="selectedPrioritySoc"
					class="prioritySoc p-2 end-0"
					:style="{ top: `${100 - bottomHeight}%` }"
					@change="changePrioritySoc"
				>
					<span class="text-decoration-underline text-nowrap pe-none">
						{{ fmtSoc(selectedPrioritySoc) }}
					</span>
				</CustomSelect>
			</div>
			<div class="progress me-md-0">
				<div
					class="bg-dark-green progress-bar text-light align-items-center"
					role="button"
					:style="{ height: `${topHeight}%` }"
					@click="toggleBufferStart"
				>
					<shopicon-regular-lightning
						size="m"
						class="icon"
						:style="iconStyle(topHeight)"
					></shopicon-regular-lightning>
				</div>
				<div
					class="bg-darker-green progress-bar text-light align-items-center"
					:style="{ height: `${middleHeight}%` }"
				>
					<shopicon-regular-car3
						size="m"
						class="icon"
						:style="iconStyle(middleHeight)"
					></shopicon-regular-car3>
				</div>
				<div
					class="bg-darkest-green progress-bar text-light align-items-center"
					:style="{ height: `${bottomHeight}%` }"
				>
					<shopicon-regular-home
						size="m"
						class="icon"
						:style="iconStyle(bottomHeight)"
					></shopicon-regular-home>
				</div>
				<div
					class="batterySoc ps-0 bg-white pe-none"
					:style="{ top: `${100 - batterySoc}%` }"
				></div>
				<div
					class="bufferStartIndicator pe-none"
					:class="{
						'bufferStartIndicator--hidden':
							!batterySolarSupport || !selectedBufferStartSoc,
					}"
					:style="{ top: `${bufferStartTop}%` }"
				>
					<div class="bufferStartIndicator__left"></div>
					<div class="bufferStartIndicator__right"></div>
				</div>
			</div>
		</div>
		<div class="col-md-12 order-md-3 col-lg-6 order-lg-1 legend pt-lg-2">
			<p class="d-flex" data-testid="battery-reserve">
				<shopicon-regular-lightning
					size="s"
					class="flex-shrink-0 me-2"
				></shopicon-regular-lightning>
				<span class="d-block">
					{{ $t("batterySettings.reserve.title") }}
					<i18n-t
						keypath="batterySettings.reserve.description"
						tag="small"
						class="d-block"
						scope="global"
					>
						<template #soc>
							<CustomSelect
								id="batterySettingsBufferTop"
								class="custom-select-inline"
								:options="reserveOptions"
								:selected="selectedReserveSoc"
								@change="changeReserveSoc"
							>
								<span class="text-decoration-underline">
									{{ fmtSoc(selectedReserveSoc) }}
								</span>
							</CustomSelect>
						</template>
					</i18n-t>

					<i18n-t
						keypath="batterySettings.solarSupport.description"
						tag="small"
						class="d-block"
						scope="global"
					>
						<template #mode>
							<CustomSelect
								id="batterySettingsSolarSupport"
								class="custom-select-inline"
								:aria-label="$t('batterySettings.solarSupport.title')"
								:options="solarSupportOptions"
								:selected="String(batterySolarSupport)"
								@change="changeBatterySolarSupport"
							>
								<span class="text-decoration-underline">
									{{ selectedSolarSupportName }}
								</span>
							</CustomSelect>
						</template>
					</i18n-t>

					<small v-if="batterySolarSupport" class="d-block">
						{{ $t("batterySettings.legendTopAutostart") }}
						<CustomSelect
							id="batterySettingsBufferStart"
							class="custom-select-inline"
							:aria-label="$t('batterySettings.legendTopAutostart')"
							:selected="selectedBufferStartSoc"
							:options="bufferStartOptions"
							@change="changeBufferStart"
						>
							<span class="text-decoration-underline">
								{{ selectedBufferStartName }}
							</span>
						</CustomSelect>
					</small>
					<small v-if="controllable" class="d-block">
						<i18n-t
							keypath="batterySettings.dischargeMode.description"
							tag="span"
							scope="global"
						>
							<template #mode>
								<CustomSelect
									id="batteryDischargeMode"
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
					</small>
					<small v-if="showReserveUnavailableNote" class="d-block">
						{{ $t("batterySettings.dischargeMode.reserveUnavailable") }}.
					</small>
				</span>
			</p>
			<p class="d-flex">
				<shopicon-regular-car3 size="s" class="flex-shrink-0 me-2"></shopicon-regular-car3>
				<span class="d-block">
					{{ $t("batterySettings.legendMiddleName") }}
					<i18n-t
						keypath="batterySettings.legendMiddleSubline"
						tag="small"
						class="d-block"
						scope="global"
					>
						<template #soc>
							<CustomSelect
								id="batterySettingsPriorityMiddle"
								class="custom-select-inline"
								:options="priorityOptions"
								:selected="selectedPrioritySoc"
								@change="changePrioritySoc"
							>
								<span class="text-decoration-underline">
									{{ fmtSoc(selectedPrioritySoc) }}
								</span>
							</CustomSelect>
						</template>
					</i18n-t>
				</span>
			</p>
			<p class="d-flex">
				<shopicon-regular-home size="s" class="flex-shrink-0 me-2"></shopicon-regular-home>
				<span class="d-block">
					{{ $t("batterySettings.legendBottomName") }}
					<i18n-t
						keypath="batterySettings.legendBottomSubline"
						tag="small"
						class="d-block"
						scope="global"
					>
						<template #soc>
							<CustomSelect
								id="batterySettingsPriorityBottom"
								class="custom-select-inline"
								:options="priorityOptions"
								:selected="selectedPrioritySoc"
								@change="changePrioritySoc"
							>
								<span class="text-decoration-underline">
									{{ fmtSoc(selectedPrioritySoc) }}
								</span>
							</CustomSelect>
						</template>
					</i18n-t>
				</span>
			</p>
		</div>
	</div>
</template>

<script lang="ts">
import "@h2d2/shopicons/es/regular/lightning";
import "@h2d2/shopicons/es/regular/car3";
import "@h2d2/shopicons/es/regular/home";
import CustomSelect from "../Helper/CustomSelect.vue";
import formatter, { POWER_UNIT } from "@/mixins/formatter";
import api from "@/api";
import { defineComponent, type PropType } from "vue";
import { BATTERY_DISCHARGE_MODE, type Battery } from "@/types/evcc";

export default defineComponent({
	name: "BatteryUsageSettings",
	components: { CustomSelect },
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
		battery: { type: Object as PropType<Battery> },
	},
	data() {
		return {
			selectedReserveSoc: 100,
			selectedPrioritySoc: 0,
			selectedBufferStartSoc: 0,
		};
	},
	computed: {
		batterySoc() {
			return this.battery?.soc ?? 0;
		},
		batteryDevices() {
			return this.battery?.devices ?? [];
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
		controllable() {
			return this.batteryDevices.some(({ controllable }) => controllable);
		},
		reserveDischargeAvailable() {
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
		showReserveUnavailableNote() {
			return (
				this.batteryDischargeMode === BATTERY_DISCHARGE_MODE.RESERVE &&
				!this.reserveDischargeAvailable
			);
		},
		selectedDischargeModeName() {
			return (
				this.dischargeModeOptions.find(({ value }) => value === this.batteryDischargeMode)
					?.name ?? ""
			);
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
		selectedSolarSupportName() {
			return this.$t(
				`batterySettings.solarSupport.${this.batterySolarSupport ? "enabled" : "disabled"}`
			);
		},
		bufferStartTop() {
			if (!this.selectedBufferStartSoc) return 0;
			return 100 - this.selectedBufferStartSoc;
		},
		bufferStartOptions() {
			const options = [];
			for (let i = 100; i >= this.selectedReserveSoc; i -= 5) {
				options.push({
					value: i,
					name: this.getBufferStartName(i),
				});
			}
			options.push({
				value: 0,
				name: this.getBufferStartName(0),
			});
			return options;
		},
		selectedBufferStartName() {
			return this.getBufferStartName(this.selectedBufferStartSoc);
		},
		topHeight() {
			return this.batterySolarSupport ? 100 - this.selectedReserveSoc : 0;
		},
		middleHeight() {
			return 100 - this.topHeight - this.bottomHeight;
		},
		bottomHeight() {
			return this.prioritySoc;
		},
		batteryDetails() {
			if (!this.batteryDevices.length) {
				return "";
			}
			const multipleBatteries = this.batteryDevices.length > 1;
			return this.batteryDevices
				.filter(({ capacity }) => capacity > 0)
				.map(({ soc = 0, capacity }) => {
					const energy = this.fmtWh(
						(capacity / 100) * soc * 1e3,
						POWER_UNIT.KW,
						!multipleBatteries,
						1
					);
					const total = this.fmtWh(capacity * 1e3, POWER_UNIT.KW, true, 1);
					const name = multipleBatteries ? "↳ " : "";
					const formattedSoc = multipleBatteries ? ` (${this.fmtSoc(soc)})` : "";
					const formattedEnergy = this.$t("batterySettings.capacity", {
						energy,
						total,
					});
					return `${name}${formattedEnergy}${formattedSoc}`;
				});
		},
	},
	watch: {
		prioritySoc(soc) {
			this.selectedPrioritySoc = soc;
		},
		batteryReserveSoc(soc) {
			this.selectedReserveSoc = soc;
		},
		bufferStartSoc(soc) {
			this.selectedBufferStartSoc = soc;
		},
	},
	mounted() {
		this.selectedReserveSoc = this.batteryReserveSoc;
		this.selectedPrioritySoc = this.prioritySoc;
		this.selectedBufferStartSoc = this.bufferStartSoc;
	},
	methods: {
		changeBufferStart($event: Event) {
			this.setBufferStartSoc(parseInt(($event.target as HTMLInputElement).value, 10));
		},
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
		toggleBufferStart() {
			const options = this.bufferStartOptions.map((option) => option.value);
			const index = options.findIndex((value) => this.bufferStartSoc >= value);
			const nextIndex = index === 0 ? options.length - 1 : index - 1;
			this.setBufferStartSoc(options[nextIndex]!);
		},
		async setBufferStartSoc(soc: number) {
			this.selectedBufferStartSoc = soc;
			return this.saveBufferStartSoc(this.selectedBufferStartSoc);
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
		async changeBatterySolarSupport($event: Event) {
			const target = $event.target as HTMLSelectElement;
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
		iconStyle(height: number) {
			let scale = 1;
			if (height <= 10) scale = 0.75;
			if (height <= 5) scale = 0;
			return { transform: `scale(${scale})` };
		},
		fmtSoc(soc: number) {
			return this.fmtPercentage(soc);
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
		getBufferStartName(value: number) {
			const key = value === 0 ? "never" : value === 100 ? "full" : "above";
			const soc = this.fmtSoc(value);
			return this.$t(`batterySettings.bufferStart.${key}`, { soc });
		},
	},
});
</script>

<style scoped>
.battery {
	height: 285px;
	display: flex;
}

.batteryLimits {
	width: 50px;
	position: relative;
}

.bufferStart,
.bufferSoc,
.prioritySoc {
	position: absolute !important;
	transform: translateY(-50%);
	transition-property: top, opacity;
	transition-timing-function: linear;
	transition-duration: var(--evcc-transition-fast);
	opacity: 1;
}

.bufferStart--hidden,
.bufferSoc--hidden {
	opacity: 0;
	pointer-events: none;
}

.batterySoc,
.bufferStartIndicator {
	position: absolute;
	transition-property: top, opacity;
	transition-timing-function: linear;
	transition-duration: var(--evcc-transition-fast);
	transform: translateY(-50%);
}
.batterySoc {
	--size: 0.7rem;
	border-radius: var(--size);
	left: 1rem;
	right: 1rem;
	height: var(--size);
	opacity: 0.8;
}

.bufferStartIndicator {
	--size: 0.7rem;
	display: flex;
	justify-content: space-between;
	left: calc(-1 * var(--size) / 4);
	right: calc(-1 * var(--size) / 4);
}
.bufferStartIndicator--hidden {
	opacity: 0;
	transform: translateY(-50%);
}
.bufferStartIndicator__left,
.bufferStartIndicator__right {
	height: var(--size);
	width: var(--size);
	background-color: var(--evcc-box);
}
.bufferStartIndicator__left {
	border-radius: 0 var(--size) var(--size) 0;
}
.bufferStartIndicator__right {
	border-radius: var(--size) 0 0 var(--size);
}
.progress {
	flex: 1;
	height: 100%;
	min-width: 100px;
	max-width: 130px;
	margin-right: 50px;
	flex-direction: column;
	position: relative;
	border-radius: 1rem;
	background-color: var(--evcc-box) !important;
}
.progress-bar {
	transition: height var(--evcc-transition-fast) linear;
}
.icon {
	transition: transform var(--evcc-transition-fast) linear;
	z-index: 1;
	border-radius: 0.5rem;
}
.custom-select-inline {
	display: inline-block !important;
}
</style>
