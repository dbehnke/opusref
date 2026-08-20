#include <opus.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>

int opus_packet_has_lbrr(const unsigned char *packet, opus_int32 length);

static int read_hex(const char *path, unsigned char *packet, int capacity) {
    FILE *input = fopen(path, "r");
    int high, low, length = 0;
    if (input == NULL) return -1;
    while ((high = fgetc(input)) != EOF && high != '\n') {
        low = fgetc(input);
        if (low == EOF || length == capacity) { fclose(input); return -1; }
        char pair[3] = {(char)high, (char)low, 0};
        packet[length++] = (unsigned char)strtoul(pair, NULL, 16);
    }
    fclose(input);
    return length;
}

int main(int argc, char **argv) {
    unsigned char context[1200], prior[1200], recovery[1200];
    opus_int16 pcm[960], reference[960], recovered[960], current[960];
    int error = OPUS_OK;
    if (argc != 4) return 2;
    int context_length = read_hex(argv[1], context, (int)sizeof(context));
    int prior_length = read_hex(argv[2], prior, (int)sizeof(prior));
    int recovery_length = read_hex(argv[3], recovery, (int)sizeof(recovery));
    if (context_length <= 0 || prior_length <= 0 || recovery_length <= 0) return 3;
    int context_lbrr = opus_packet_has_lbrr(context, context_length);
    int prior_lbrr = opus_packet_has_lbrr(prior, prior_length);
    int recovery_lbrr = opus_packet_has_lbrr(recovery, recovery_length);
    if (recovery_lbrr != 1) return 3;
    OpusDecoder *decoder = opus_decoder_create(48000, 1, &error);
    if (decoder == NULL || error != OPUS_OK) return 3;
    if (opus_decode(decoder, context, context_length, pcm, 960, 0) != 960) return 4;
    if (opus_decode(decoder, prior, prior_length, reference, 960, 0) != 960) return 5;
    if (opus_decoder_ctl(decoder, OPUS_RESET_STATE) != OPUS_OK) return 6;
    if (opus_decode(decoder, context, context_length, pcm, 960, 0) != 960) return 7;
    if (opus_decode(decoder, recovery, recovery_length, recovered, 960, 1) != 960) return 8;
    if (opus_decode(decoder, recovery, recovery_length, current, 960, 0) != 960) return 9;
    int64_t energy = 0, prior_error = 0, current_error = 0;
    for (int index = 0; index < 960; index++) {
        energy += recovered[index] < 0 ? -(int64_t)recovered[index] : recovered[index];
        int64_t prior_delta = (int64_t)recovered[index] - reference[index];
        int64_t current_delta = (int64_t)recovered[index] - current[index];
        prior_error += prior_delta * prior_delta;
        current_error += current_delta * current_delta;
    }
    if (energy < 1000 || prior_error >= current_error) return 10;
    printf("context_lbrr=%d prior_lbrr=%d recovery_lbrr=%d recovered_samples=960\n", context_lbrr, prior_lbrr, recovery_lbrr);
    opus_decoder_destroy(decoder);
    return 0;
}
