package probe;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertInstanceOf;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assumptions.assumeTrue;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonSubTypes;
import com.fasterxml.jackson.core.JsonPointer;
import com.fasterxml.jackson.databind.DeserializationFeature;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.exc.InvalidTypeIdException;
import com.fasterxml.jackson.databind.exc.MismatchedInputException;
import com.fasterxml.jackson.databind.exc.UnrecognizedPropertyException;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
import java.io.File;
import java.util.Arrays;
import java.util.Comparator;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.DynamicTest;
import org.junit.jupiter.api.TestFactory;
import org.openapitools.client.JSON;
import org.openapitools.client.model.AuditEventDto;
import org.openapitools.client.model.EntityAuditEventsResponseDto;
import org.openapitools.client.model.StateMachineAuditEventDto;
import org.openapitools.client.model.StateMachineEventDto;
import org.opentest4j.TestAbortedException;

// Reads and writes every sample with the classes the java generator makes
// (library native, Jackson; package org.openapitools.client.model).
public class JavaSamplesTest {
    private final ObjectMapper mapper =
            new JSON().getMapper().copy().setSerializationInclusion(JsonInclude.Include.NON_NULL);

    // The two settings Rule 6 requires of a client: unknown fields and unknown
    // discriminator values are read as nothing.
    private final ObjectMapper tolerant = mapper.copy()
            .disable(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES)
            .disable(DeserializationFeature.FAIL_ON_INVALID_SUBTYPE);

    // The generated mapper already disables FAIL_ON_UNKNOWN_PROPERTIES.
    private final ObjectMapper strict = mapper.copy()
            .enable(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES)
            .enable(DeserializationFeature.FAIL_ON_INVALID_SUBTYPE);

    @TestFactory
    public List<DynamicTest> samples() {
        File dir = new File(System.getProperty("samples.dir", "samples"));
        File[] files = dir.listFiles((d, name) -> name.endsWith(".json"));
        assertTrue(files != null && files.length > 0, "no samples in " + dir);
        return Arrays.stream(files)
                .sorted(Comparator.comparing(File::getName))
                .map(f -> DynamicTest.dynamicTest(f.getName(), () -> {
                    try {
                        check(mapper.readTree(f));
                    } catch (TestAbortedException e) {
                        throw e;
                    } catch (Throwable e) {
                        throw new AssertionError(f.getName() + ": " + e.getMessage(), e);
                    }
                }))
                .toList();
    }

    private void check(JsonNode sample) throws Exception {
        // A create or import body is not a component schema: the generated
        // clients take it as an untyped value, so there is no class to read it as.
        assumeTrue(!sample.has("body"), "a request body sample");
        assumeTrue(sample.path("valid").asBoolean(true), "an invalid sample");
        JsonNode value = sample.path("value");
        Class<?> cls = Class.forName("org.openapitools.client.model." + sample.path("schema").asText());
        if (sample.path("future").asBoolean(false)) {
            future(cls, sample);
            return;
        }
        String expected = sample.path("class").textValue();
        assertNotNull(expected, "a valid sample names its class");
        Object read = mapper.readValue(value.toString(), cls);
        assertEquals(expected, read.getClass().getSimpleName(), "the class read");
        assertEquals(withoutNullMembers(value),
                withoutNullMembers(mapper.readTree(mapper.writeValueAsString(read))), "the value written back");
    }

    // A future sample is an audit page that a later spec could send. Rule 6:
    // read with the two settings, an unknown audit kind is a null entry in
    // items, an unknown event type keeps eventType and has null data, and a
    // known event is read with exactly the sample's added members dropped.
    // Without the two settings, the read fails.
    private void future(Class<?> cls, JsonNode sample) throws Exception {
        assertEquals(EntityAuditEventsResponseDto.class, cls, "a future sample is an audit page");
        JsonNode value = sample.path("value");
        JsonNode declared = withoutAdded(value, sample.path("added"));
        EntityAuditEventsResponseDto page = tolerant.readValue(value.toString(), EntityAuditEventsResponseDto.class);
        List<AuditEventDto> items = page.getItems();
        assertNotNull(items);
        assertEquals(value.path("items").size(), items.size(), "the number of items");
        boolean unknownMember = false;
        for (int i = 0; i < items.size(); i++) {
            JsonNode sent = value.path("items").get(i);
            AuditEventDto item = items.get(i);
            if (!known(AuditEventDto.class, sent.path("auditEventType").asText())) {
                unknownMember = true;
                assertNull(item, "an audit event of an unknown kind");
            } else if (sent.path("auditEventType").asText().equals("StateMachine")
                    && !known(StateMachineEventDto.class, sent.path("data").path("eventType").asText())) {
                unknownMember = true;
                StateMachineAuditEventDto event = assertInstanceOf(StateMachineAuditEventDto.class, item);
                assertEquals(sent.path("eventType").asText(), event.getEventType(), "the event type");
                assertNull(event.getData(), "the data of an unknown event type");
            } else {
                assertNotNull(item, "a known audit event");
                JsonNode written = tolerant.readTree(tolerant.writeValueAsString(item));
                assertEquals(withoutNullMembers(declared.path("items").get(i)), withoutNullMembers(written),
                        "the known event written back");
            }
        }
        Class<? extends MismatchedInputException> failure =
                unknownMember ? InvalidTypeIdException.class : UnrecognizedPropertyException.class;
        assertThrows(failure, () -> strict.readValue(value.toString(), EntityAuditEventsResponseDto.class),
                "the read without the two settings");
    }

    // withoutAdded returns value with the members at the JSON pointers in
    // added removed.
    private static JsonNode withoutAdded(JsonNode value, JsonNode added) {
        JsonNode out = value.deepCopy();
        for (JsonNode a : added) {
            JsonPointer ptr = JsonPointer.compile(a.asText());
            ObjectNode parent = assertInstanceOf(ObjectNode.class, out.at(ptr.head()), "added " + a.asText() + ": no object holds it");
            assertNotNull(parent.remove(ptr.last().getMatchingProperty()), "added " + a.asText() + ": no such member");
        }
        return out;
    }

    private static boolean known(Class<?> base, String name) {
        return Arrays.stream(base.getAnnotation(JsonSubTypes.class).value()).anyMatch(t -> t.name().equals(name));
    }

    // Cloud writes non_null, and for these fields a null member and an absent
    // one mean the same: drop object members whose value is null, keep null
    // array entries.
    private JsonNode withoutNullMembers(JsonNode node) {
        if (node instanceof ObjectNode object) {
            ObjectNode out = mapper.createObjectNode();
            for (Map.Entry<String, JsonNode> e : object.properties()) {
                if (!e.getValue().isNull()) {
                    out.set(e.getKey(), withoutNullMembers(e.getValue()));
                }
            }
            return out;
        }
        if (node instanceof ArrayNode array) {
            ArrayNode out = mapper.createArrayNode();
            array.forEach(e -> out.add(withoutNullMembers(e)));
            return out;
        }
        return node;
    }
}
