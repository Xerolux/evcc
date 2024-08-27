package ocpp

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/remotetrigger"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
)

const desiredMeasurands = "Power.Active.Import,Energy.Active.Import.Register,Current.Import,Voltage,Current.Offered,Power.Offered,SoC"

func (cp *CP) Setup() error {
	if err := Instance().ChangeAvailabilityRequest(cp.ID(), 0, core.AvailabilityTypeOperative); err != nil {
		cp.log.DEBUG.Printf("failed configuring availability: %v", err)
	}

	var meterValuesSampledData string
	meterValuesSampledDataMaxLength := len(strings.Split(desiredMeasurands, ","))

	rc := make(chan error, 1)

	// CP
	err := Instance().GetConfiguration(cp.ID(), func(resp *core.GetConfigurationConfirmation, err error) {
		if err == nil {
			for _, opt := range resp.ConfigurationKey {
				if opt.Value == nil {
					continue
				}

				switch opt.Key {
				case KeyChargeProfileMaxStackLevel:
					if val, err := strconv.Atoi(*opt.Value); err == nil {
						cp.StackLevel = val
					}

				case KeyChargingScheduleAllowedChargingRateUnit:
					if *opt.Value == "Power" || *opt.Value == "W" { // "W" is not allowed by spec but used by some CPs
						cp.ChargingRateUnit = types.ChargingRateUnitWatts
					}

				case KeyConnectorSwitch3to1PhaseSupported:
					var val bool
					if val, err = strconv.ParseBool(*opt.Value); err == nil {
						cp.PhaseSwitching = val
					}

				case KeyMaxChargingProfilesInstalled:
					if val, err := strconv.Atoi(*opt.Value); err == nil {
						cp.ChargingProfileId = val
					}

				case KeyMeterValuesSampledData:
					if opt.Readonly {
						meterValuesSampledDataMaxLength = 0
					}
					meterValuesSampledData = *opt.Value

				case KeyMeterValuesSampledDataMaxLength:
					if val, err := strconv.Atoi(*opt.Value); err == nil {
						meterValuesSampledDataMaxLength = val
					}

				case KeyNumberOfConnectors:
					var val int
					if val, err = strconv.Atoi(*opt.Value); err == nil && connector > val {
						err = fmt.Errorf("connector %d exceeds max available connectors: %d", connector, val)
					}

				case KeySupportedFeatureProfiles:
					if !hasProperty(*opt.Value, smartcharging.ProfileName) {
						cp.log.WARN.Printf("the required SmartCharging feature profile is not indicated as supported")
					}
					// correct the availability assumption of RemoteTrigger only in case of a valid looking FeatureProfile list
					if hasProperty(*opt.Value, core.ProfileName) {
						cp.HasRemoteTriggerFeature = hasProperty(*opt.Value, remotetrigger.ProfileName)
					}

				// vendor-specific keys
				case KeyAlfenPlugAndChargeIdentifier:
					if cp.idtag == defaultIdTag {
						cp.idtag = *opt.Value
						cp.log.DEBUG.Printf("overriding default `idTag` with Alfen-specific value: %s", cp.idtag)
					}

				case KeyEvBoxSupportedMeasurands:
					if meterValues == "" {
						meterValues = *opt.Value
					}
				}

				if err != nil {
					break
				}
			}
		}

		rc <- err
	}, nil)

	if err := cp.wait(err, rc); err != nil {
		return nil, err
	}

	// see who's there
	if cp.HasRemoteTriggerFeature {
		// CP
		if err := Instance().TriggerMessageRequest(cp.ID(), core.BootNotificationFeatureName); err != nil {
			cp.log.DEBUG.Printf("failed triggering BootNotification: %v", err)
		}

		select {
		case <-time.After(timeout):
			cp.log.DEBUG.Printf("BootNotification timeout")
		case res := <-cp.BootNotificationRequest():
			if res != nil {
				cp.bootNotification = res
			}
		}
	}

	// autodetect measurands
	if meterValues == "" && meterValuesSampledDataMaxLength > 0 {
		sampledMeasurands := cp.tryMeasurands(desiredMeasurands, KeyMeterValuesSampledData)
		meterValues = strings.Join(sampledMeasurands[:min(len(sampledMeasurands), meterValuesSampledDataMaxLength)], ",")
	}

	// configure measurands
	if meterValues != "" {
		// CP
		if err := cp.configure(KeyMeterValuesSampledData, meterValues); err == nil {
			meterValuesSampledData = meterValues
		}
	}

	cp.meterValuesSample = meterValuesSampledData

	// trigger initial meter values
	if cp.HasRemoteTriggerFeature {
		// CP
		if err := conn.TriggerMessageRequest(core.MeterValuesFeatureName); err == nil {
			// wait for meter values
			select {
			case <-time.After(timeout):
				cp.log.WARN.Println("meter timeout")
			case <-cp.conn.MeterSampled():
			}
		}
	}

	// configure sample rate
	if meterInterval > 0 {
		// CP
		if err := cp.configure(KeyMeterValueSampleInterval, strconv.Itoa(int(meterInterval.Seconds()))); err != nil {
			cp.log.WARN.Printf("failed configuring MeterValueSampleInterval: %v", err)
		}
	}

	// configure ping interval
	// CP
	cp.configure(KeyWebSocketPingInterval, "30")
}

// HasMeasurement checks if meterValuesSample contains given measurement
func (cp *CP) HasMeasurement(val types.Measurand) bool {
	return hasProperty(cp.meterValuesSample, string(val))
}

func (cp *CP) tryMeasurands(measurands string, key string) []string {
	var accepted []string
	for _, m := range strings.Split(measurands, ",") {
		if err := cp.configure(key, m); err == nil {
			accepted = append(accepted, m)
		}
	}
	return accepted
}

// configure updates CP configuration
func (cp *CP) configure(key, val string) error {
	rc := make(chan error, 1)

	err := Instance().ChangeConfiguration(cp.id, func(resp *core.ChangeConfigurationConfirmation, err error) {
		if err == nil && resp != nil && resp.Status != core.ConfigurationStatusAccepted {
			rc <- fmt.Errorf("ChangeConfiguration failed: %s", resp.Status)
		}

		rc <- err
	}, key, val)

	return c.wait(err, rc)
}