#include <opus.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

int opus_packet_has_lbrr(const unsigned char *packet, opus_int32 length);

static int write_hex(const char *path, const unsigned char *packet, int length) {
    FILE *output = fopen(path, "w");
    if (output == NULL) return 0;
    for (int index = 0; index < length; index++) fprintf(output, "%02x", packet[index]);
    fputc('\n', output);
    return fclose(output) == 0;
}

int main(int argc, char **argv) {
    unsigned char packets[40][400];
    int lengths[40] = {0};
    opus_int16 pcm[320];
    uint32_t noise = 1;
    int error = OPUS_OK;
    if (argc != 4) return 2;
    if (strcmp(opus_get_version_string(), "libopus 1.6.1") != 0) return 8;
    OpusEncoder *encoder = opus_encoder_create(16000, 1, OPUS_APPLICATION_VOIP, &error);
    if (encoder == NULL || error != OPUS_OK) return 3;
    if (opus_encoder_ctl(encoder, OPUS_SET_BITRATE(24000)) != OPUS_OK ||
        opus_encoder_ctl(encoder, OPUS_SET_VBR(1)) != OPUS_OK ||
        opus_encoder_ctl(encoder, OPUS_SET_INBAND_FEC(1)) != OPUS_OK ||
        opus_encoder_ctl(encoder, OPUS_SET_PACKET_LOSS_PERC(50)) != OPUS_OK ||
        opus_encoder_ctl(encoder, OPUS_SET_SIGNAL(OPUS_SIGNAL_VOICE)) != OPUS_OK ||
        opus_encoder_ctl(encoder, OPUS_SET_BANDWIDTH(OPUS_BANDWIDTH_WIDEBAND)) != OPUS_OK) return 4;
    for (int frame = 0; frame < 40; frame++) {
        for (int index = 0; index < 320; index++) {
            noise ^= noise << 13; noise ^= noise >> 17; noise ^= noise << 5;
            int period = 55 + frame % 7;
            int voiced = index % period < period / 2 ? 5000 : -5000;
            pcm[index] = (opus_int16)(voiced + (int)(noise & 1023) - 512);
        }
        lengths[frame] = opus_encode(encoder, pcm, 320, packets[frame], 400);
        if (lengths[frame] < 1) return 5;
        if (frame >= 2 && opus_packet_has_lbrr(packets[frame], lengths[frame]) == 1) {
            int ok = write_hex(argv[1], packets[frame - 2], lengths[frame - 2]) &&
                     write_hex(argv[2], packets[frame - 1], lengths[frame - 1]) &&
                     write_hex(argv[3], packets[frame], lengths[frame]);
            opus_encoder_destroy(encoder);
            return ok ? 0 : 6;
        }
    }
    opus_encoder_destroy(encoder);
    return 7;
}
