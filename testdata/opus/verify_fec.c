#include <opus.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>

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
    unsigned char prior[1200], recovery[1200];
    opus_int16 pcm[960];
    int error = OPUS_OK;
    if (argc != 3) return 2;
    int prior_length = read_hex(argv[1], prior, (int)sizeof(prior));
    int recovery_length = read_hex(argv[2], recovery, (int)sizeof(recovery));
    OpusDecoder *decoder = opus_decoder_create(48000, 1, &error);
    if (decoder == NULL || error != OPUS_OK || prior_length <= 0 || recovery_length <= 0) return 3;
    if (opus_decode(decoder, prior, prior_length, pcm, 960, 0) != 960) return 4;
    if (opus_decoder_ctl(decoder, OPUS_RESET_STATE) != OPUS_OK) return 5;
    if (opus_decode(decoder, NULL, 0, pcm, 960, 0) != 960) return 6;
    if (opus_decode(decoder, recovery, recovery_length, pcm, 960, 1) != 960) return 7;
	int64_t energy = 0;
	for (int index = 0; index < 960; index++) {
		energy += pcm[index] < 0 ? -(int64_t)pcm[index] : pcm[index];
	}
	if (energy < 1000) return 8;
    opus_decoder_destroy(decoder);
    return 0;
}
