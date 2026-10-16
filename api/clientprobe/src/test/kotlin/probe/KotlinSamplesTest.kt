package probe

import com.fasterxml.jackson.annotation.JsonInclude
import com.fasterxml.jackson.annotation.JsonSubTypes
import com.fasterxml.jackson.databind.DeserializationFeature
import com.fasterxml.jackson.databind.JsonNode
import com.fasterxml.jackson.databind.ObjectMapper
import com.fasterxml.jackson.databind.SerializationFeature
import com.fasterxml.jackson.databind.node.ArrayNode
import com.fasterxml.jackson.databind.node.ObjectNode
import com.fasterxml.jackson.module.kotlin.registerKotlinModule
import org.junit.jupiter.api.Assumptions.assumeTrue
import org.junit.jupiter.api.DynamicTest
import org.junit.jupiter.api.TestFactory
import org.opentest4j.TestAbortedException
import org.openapitools.model.AuditEventDto
import org.openapitools.model.EntityAuditEventsResponseDto
import org.openapitools.model.StateMachineAuditEventDto
import org.openapitools.model.StateMachineEventDto
import java.io.File
import java.util.Objects
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertNotNull
import kotlin.test.assertNull
import kotlin.test.assertTrue
import kotlin.test.fail

// Reads and writes every sample with the classes kotlin-spring generates with
// Cloud's options (package org.openapitools.model).
class KotlinSamplesTest {
    private val mapper: ObjectMapper = ObjectMapper().registerKotlinModule().findAndRegisterModules()
        .setSerializationInclusion(JsonInclude.Include.NON_NULL)
        .disable(SerializationFeature.WRITE_DATES_AS_TIMESTAMPS)
        .disable(DeserializationFeature.ADJUST_DATES_TO_CONTEXT_TIME_ZONE)

    // The two settings Rule 6 requires of a client: unknown fields and unknown
    // discriminator values are read as nothing.
    private val tolerant: ObjectMapper = mapper.copy()
        .disable(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES)
        .disable(DeserializationFeature.FAIL_ON_INVALID_SUBTYPE)

    private val strict: ObjectMapper = mapper.copy()
        .enable(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES)
        .enable(DeserializationFeature.FAIL_ON_INVALID_SUBTYPE)

    @TestFactory
    fun samples(): List<DynamicTest> {
        val dir = File(System.getProperty("samples.dir") ?: "samples")
        val files = dir.listFiles { f -> f.name.endsWith(".json") }?.sortedBy { it.name }
        assertTrue(!files.isNullOrEmpty(), "no samples in $dir")
        return files.map { f ->
            DynamicTest.dynamicTest(f.name) {
                try {
                    check(mapper.readTree(f))
                } catch (e: TestAbortedException) {
                    throw e
                } catch (e: Throwable) {
                    throw AssertionError("${f.name}: ${e.message}", e)
                }
            }
        }
    }

    private fun check(sample: JsonNode) {
        // A create or import body is not a component schema: the generated
        // clients take it as an untyped value, so there is no class to read it as.
        assumeTrue(!sample.has("body"), "a request body sample")
        assumeTrue(sample.path("valid").asBoolean(true), "an invalid sample")
        val value = sample.path("value")
        val cls = Class.forName("org.openapitools.model." + sample.path("schema").asText())
        if (sample.path("future").asBoolean(false)) {
            future(cls, value)
            return
        }
        val expected = assertNotNull(sample.path("class").textValue(), "a valid sample names its class")
        val read: Any = mapper.readValue(value.toString(), cls)
        assertEquals(expected, read.javaClass.simpleName, "the class read")
        assertEquals(withoutNullMembers(value), withoutNullMembers(mapper.readTree(mapper.writeValueAsString(read))), "the value written back")
    }

    // A future sample is an audit page that a later spec could send. Rule 6:
    // read with the two settings, an unknown audit kind is a null entry in
    // items, an unknown event type keeps eventType and has null data, and an
    // added field is ignored. Without them, the read fails.
    private fun future(cls: Class<*>, value: JsonNode) {
        assertEquals(EntityAuditEventsResponseDto::class.java, cls, "a future sample is an audit page")
        val page = tolerant.readValue(value.toString(), EntityAuditEventsResponseDto::class.java)
        val items = assertNotNull(page.items)
        assertEquals(value.path("items").size(), items.size, "the number of items")
        value.path("items").forEachIndexed { i, sent ->
            val item: AuditEventDto? = items[i]
            when {
                !known(AuditEventDto::class.java, sent.path("auditEventType").asText()) ->
                    assertNull(item, "an audit event of an unknown kind")
                sent.path("auditEventType").asText() == "StateMachine" &&
                    !known(StateMachineEventDto::class.java, sent.path("data").path("eventType").asText()) -> {
                    val event = item as? StateMachineAuditEventDto ?: fail("a state-machine event, got $item")
                    assertEquals(sent.path("eventType").asText(), event.eventType, "the event type")
                    // The generated property is non-null; the read leaves it null.
                    assertTrue(Objects.isNull(event.data), "the data of an unknown event type")
                }
                else -> {
                    assertNotNull(item, "a known audit event")
                    val written = withoutNullMembers(tolerant.readTree(tolerant.writeValueAsString(item)))
                    assertTrue(subset(written, sent), "the known fields: wrote $written for $sent")
                }
            }
        }
        assertFailsWith<Exception>("the read without the two settings") {
            strict.readValue(value.toString(), EntityAuditEventsResponseDto::class.java)
        }
    }

    private fun known(base: Class<*>, name: String): Boolean =
        base.getAnnotation(JsonSubTypes::class.java).value.any { it.name == name }

    // Cloud writes non_null, and for these fields a null member and an absent
    // one mean the same: drop object members whose value is null, keep null
    // array entries.
    private fun withoutNullMembers(node: JsonNode): JsonNode = when (node) {
        is ObjectNode -> mapper.createObjectNode().also { out ->
            node.properties().forEach { (k, v) -> if (!v.isNull) out.set<JsonNode>(k, withoutNullMembers(v)) }
        }
        is ArrayNode -> mapper.createArrayNode().also { out -> node.forEach { out.add(withoutNullMembers(it)) } }
        else -> node
    }

    // Every member of a is in b with a value that is itself a subset.
    private fun subset(a: JsonNode, b: JsonNode): Boolean = when {
        a is ObjectNode && b is ObjectNode -> a.properties().all { (k, v) -> b.has(k) && subset(v, b.get(k)) }
        a is ArrayNode && b is ArrayNode -> a.size() == b.size() && (0 until a.size()).all { subset(a[it], b[it]) }
        else -> a == b
    }
}
