interface AudioEncoderConfig { codec: string; sampleRate: number; numberOfChannels: number; bitrate?: number }
interface AudioDecoderConfig { codec: string; sampleRate: number; numberOfChannels: number }
interface AudioEncoderConstructor { isConfigSupported(config: AudioEncoderConfig): Promise<{ supported?: boolean }> }
interface AudioDecoderConstructor { isConfigSupported(config: AudioDecoderConfig): Promise<{ supported?: boolean }> }
declare const AudioEncoder: AudioEncoderConstructor
declare const AudioDecoder: AudioDecoderConstructor
